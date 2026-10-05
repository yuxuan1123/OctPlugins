package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 本文件：模型首次下载（转化域 §26「首次下载须同意 + 后台拉取 + 进度，**下载由宿主执行**」）。
//
// 职责边界：
//   - 宿主负责下载（本文件），插件与 tool 只声明 downloadUrl、不自行拉取权重（§四）；
//   - 下载目标遵循 §20.5 的 <modelsRoot>/<modelId>/<quant>/ 布局；
//   - 声明了 downloadSha256 时 MUST 校验，不匹配则删除产物并报错（不留半成品）。

// DownloadProgress 一次下载的进度快照（经 models.download.* 事件推给宿主 UI）。
type DownloadProgress struct {
	ModelID   string  `json:"modelId"`
	Phase     string  `json:"phase"` // started | progress | verifying | done | failed
	URL       string  `json:"url,omitempty"`
	Dest      string  `json:"dest,omitempty"`
	Bytes     int64   `json:"bytes"`
	Total     int64   `json:"total,omitempty"` // 服务端未给 Content-Length 时为 0
	Percent   float64 `json:"percent,omitempty"`
	Err       string  `json:"err,omitempty"`
	ElapsedMs int64   `json:"elapsedMs,omitempty"`
}

// downloadTimeout 单次模型下载的上限（大模型 + 慢网；不做断点续传，超时即失败可重试）。
const downloadTimeout = 60 * time.Minute

// PullModel 下载某模型的权重到 <modelsRoot>/<modelId>/<quant>/（§26，下载由宿主执行）。
//
// 调用方 MUST 已获得用户同意（UI 侧弹确认）：本函数只负责执行，不做二次征询。
// onProgress 可为 nil；每次有意义的进展都会回调（用于广播进度事件）。
func (m *Manager) PullModel(ctx context.Context, modelID string, onProgress func(DownloadProgress)) (string, error) {
	me, ok := m.modelByID(modelID)
	if !ok {
		return "", fmt.Errorf("model %q not registered", modelID)
	}
	if strings.TrimSpace(me.DownloadURL) == "" {
		return "", fmt.Errorf(
			"模型 %s 未声明 downloadUrl（providesModels.downloadUrl），宿主无法自动下载；"+
				"请手动把权重放到 %s", modelID, m.ResolveModelPath(me.Path))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	emit := func(p DownloadProgress) {
		if onProgress != nil {
			onProgress(p)
		}
	}

	// §20.5：<modelsRoot>/<modelId>/<quant>/
	quant := strings.TrimSpace(me.Quant)
	if quant == "" {
		quant = "default"
	}
	destDir := filepath.Join(m.ModelsRootDir(), modelID, quant)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("创建模型目录 %s: %w", destDir, err)
	}
	name := downloadFileName(me.DownloadURL, modelID)
	dest := filepath.Join(destDir, name)

	started := time.Now()
	emit(DownloadProgress{ModelID: modelID, Phase: "started", URL: me.DownloadURL, Dest: dest})

	req, err := http.NewRequestWithContext(dctx, http.MethodGet, me.DownloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("构造下载请求: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		emit(DownloadProgress{ModelID: modelID, Phase: "failed", Err: err.Error()})
		return "", fmt.Errorf("下载 %s: %w", me.DownloadURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("下载 %s: HTTP %d", me.DownloadURL, resp.StatusCode)
		emit(DownloadProgress{ModelID: modelID, Phase: "failed", Err: err.Error()})
		return "", err
	}
	total := resp.ContentLength

	// 先写 .part，校验通过再改名——避免中断留下看似完整的半个文件。
	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return "", fmt.Errorf("创建 %s: %w", part, err)
	}
	hasher := sha256.New()
	buf := make([]byte, 1<<20)
	var written int64
	var lastEmit time.Time
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(part)
				return "", fmt.Errorf("写入 %s: %w", part, werr)
			}
			hasher.Write(buf[:n])
			written += int64(n)
			// 节流：最多每 200ms 报一次进度，避免高频事件打爆 WS。
			if time.Since(lastEmit) > 200*time.Millisecond {
				lastEmit = time.Now()
				p := DownloadProgress{
					ModelID: modelID, Phase: "progress", URL: me.DownloadURL, Dest: dest,
					Bytes: written, Total: total, ElapsedMs: time.Since(started).Milliseconds(),
				}
				if total > 0 {
					p.Percent = float64(written) / float64(total) * 100
				}
				emit(p)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(part)
			emit(DownloadProgress{ModelID: modelID, Phase: "failed", Bytes: written, Err: rerr.Error()})
			return "", fmt.Errorf("读取响应: %w", rerr)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return "", fmt.Errorf("关闭 %s: %w", part, err)
	}

	// 声明了 downloadSha256 就 MUST 校验（§20 同源要求：模型也是外部资源）。
	if want := strings.ToLower(strings.TrimSpace(me.DownloadSHA256)); want != "" {
		emit(DownloadProgress{ModelID: modelID, Phase: "verifying", Bytes: written, Total: total})
		got := hex.EncodeToString(hasher.Sum(nil))
		if got != want {
			os.Remove(part)
			err := fmt.Errorf("模型 %s 下载后 sha256 不匹配（期望 %s，实际 %s），已丢弃产物",
				modelID, want, got)
			emit(DownloadProgress{ModelID: modelID, Phase: "failed", Bytes: written, Err: err.Error()})
			return "", err
		}
	}
	if err := os.Rename(part, dest); err != nil {
		os.Remove(part)
		return "", fmt.Errorf("落盘 %s: %w", dest, err)
	}

	// §20.5/§12：把登记里的 path 指向新下载的权重，后续 Acquire 直接用。
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rs != nil {
		cur, ok := rs.Model(modelID)
		if ok {
			cur.Path = dest
			cur.State = "registered"
			if err := rs.SetModel(modelID, cur); err != nil {
				return dest, fmt.Errorf("更新 registry 模型路径: %w", err)
			}
		}
	}

	emit(DownloadProgress{
		ModelID: modelID, Phase: "done", URL: me.DownloadURL, Dest: dest,
		Bytes: written, Total: total, Percent: 100, ElapsedMs: time.Since(started).Milliseconds(),
	})
	return dest, nil
}

// downloadFileName 从 URL 推断落盘文件名；无可用末段时退回 <modelId>.bin。
func downloadFileName(rawURL, modelID string) string {
	trimmed := strings.TrimRight(rawURL, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 && i+1 < len(trimmed) {
		base := trimmed[i+1:]
		if q := strings.IndexByte(base, '?'); q >= 0 {
			base = base[:q]
		}
		if base != "" && base != "." && base != ".." {
			return base
		}
	}
	return modelID + ".bin"
}
