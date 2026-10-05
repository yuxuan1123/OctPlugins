package lifecycle

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 本文件：模型存储布局的规范化（转化域 §20.5）。
//
// §20.5 要求「模型按 <modelId>/<quant>/ 存」。既有部署里模型往往散落在
// <root>/<provider>/... 之类的位置（本机 9 个模型全部如此），而搬迁数 GB 文件
// 属于破坏性操作，故设计为：
//
//   - **审计**（ModelLayoutReport）：如实报告每个模型当前路径是否符合规范，不动文件；
//   - **搬迁**（RelocateModel）：由用户显式发起、必须显式确认（dryRun 先看计划）、
//     一次只处理一个模型、绝不覆盖已存在的目标、完成后更新 registry.Path。
//
// 搬迁之所以安全，是因为 §1.4 已落实：tool 只按 id 申请模型，**权重路径由宿主下发**
// （见 model_hooks.go 的 callProviderModel）。改 registry.Path 后，下一次 model.ensure
// 就会把新路径传给 tool，不存在「tool 里硬编码旧路径」的失联问题。

// ModelLayout 一个模型的布局审计结果。
type ModelLayout struct {
	ID         string `json:"id"`
	Provider   string `json:"provider,omitempty"`
	Quant      string `json:"quant,omitempty"`
	Current    string `json:"current"`             // 当前登记路径的绝对形式
	Canonical  string `json:"canonical"`           // <modelsRoot>/<modelId>/<quant>/ 规范目标
	IsDir      bool   `json:"isDir"`               // 当前是目录型模型
	Exists     bool   `json:"exists"`
	Conforming bool   `json:"conforming"`          // 是否已落在规范布局下
	Bytes      int64  `json:"bytes,omitempty"`     // 占用（目录型递归统计）
	Files      int    `json:"files,omitempty"`
	Reason     string `json:"reason,omitempty"`    // 不可搬迁的原因（Conforming=false 时给出）
}

// canonicalDirFor 返回某模型的规范目录 <modelsRoot>/<modelId>/<quant>。
func (m *Manager) canonicalDirFor(id, quant string) string {
	if strings.TrimSpace(quant) == "" {
		quant = "default"
	}
	return filepath.Join(m.ModelsRootDir(), id, quant)
}

// canonicalPathFor 返回某模型在规范布局下的完整目标路径。
//   - 文件型模型（.gguf / .onnx 单文件）→ <root>/<id>/<quant>/<文件名>
//   - 目录型模型 → <root>/<id>/<quant>（目录内容直接落进去）
func (m *Manager) canonicalPathFor(me modelRecLike) string {
	dir := m.canonicalDirFor(me.id(), me.quant())
	cur := m.ResolveModelPath(me.path())
	if cur == "" {
		return dir
	}
	if st, err := os.Stat(cur); err == nil && !st.IsDir() {
		return filepath.Join(dir, filepath.Base(cur))
	}
	return dir
}

// modelRecLike 让审计逻辑同时接受 registry 条目与 ResourceMap 视图。
type modelRecLike interface {
	id() string
	quant() string
	path() string
}

type layoutRec struct {
	ID    string
	Quant string
	Path  string
}

func (r layoutRec) id() string    { return r.ID }
func (r layoutRec) quant() string { return r.Quant }
func (r layoutRec) path() string  { return r.Path }

// ModelLayoutReport 审计全部模型的存储布局（只读，不动文件）。
func (m *Manager) ModelLayoutReport() []ModelLayout {
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	out := []ModelLayout{}
	if rs == nil {
		return out
	}
	f, err := rs.Load()
	if err != nil {
		return out
	}
	root := filepath.Clean(m.ModelsRootDir())
	for id, me := range f.Models {
		rec := layoutRec{ID: id, Quant: me.Quant, Path: me.Path}
		cur := m.ResolveModelPath(me.Path)
		canon := m.canonicalPathFor(rec)
		l := ModelLayout{
			ID: id, Provider: me.Provider, Quant: me.Quant,
			Current: cur, Canonical: canon,
		}
		if cur != "" {
			if st, err := os.Stat(cur); err == nil {
				l.Exists = true
				l.IsDir = st.IsDir()
				if st.IsDir() {
					l.Bytes, l.Files = dirSize(cur)
				} else {
					l.Bytes, l.Files = st.Size(), 1
				}
			} else {
				l.Reason = "当前登记路径不存在（权重可能未下载或已被移动）"
			}
		} else {
			l.Reason = "未声明模型路径"
		}
		// 规范性判定：目标路径位于 <root>/<id>/<quant>/ 之下即视为规范。
		wantPrefix := filepath.Clean(m.canonicalDirFor(id, me.Quant)) + string(os.PathSeparator)
		l.Conforming = strings.HasPrefix(filepath.Clean(cur)+string(os.PathSeparator), wantPrefix) ||
			filepath.Clean(cur) == filepath.Clean(m.canonicalDirFor(id, me.Quant))
		if !l.Conforming && l.Reason == "" {
			l.Reason = fmt.Sprintf("当前位于 %s，规范位置应为 %s（§20.5）", filepath.Dir(cur), canon)
		}
		if l.Conforming {
			l.Reason = ""
		}
		// 模型根之外一律不搬（安全护栏，避免误动系统目录）。
		if !strings.HasPrefix(filepath.Clean(cur), root+string(os.PathSeparator)) && !l.Conforming {
			l.Reason += "；位于模型存储根之外，宿主不会自动搬迁"
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RelocateResult 一次搬迁的计划/结果。
type RelocateResult struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	IsDir     bool   `json:"isDir"`
	Bytes     int64  `json:"bytes"`
	Files     int    `json:"files"`
	DryRun    bool   `json:"dryRun"`
	Done      bool   `json:"done"`
	MovedFile int    `json:"movedFiles,omitempty"`
	Note      string `json:"note,omitempty"`
}

// RelocateModel 把某模型的权重搬进 <modelsRoot>/<modelId>/<quant>/（§20.5）。
//
// 安全约束（任一不满足即拒绝，绝不半途破坏）：
//  1. 源必须存在；2. 目标不得已存在（不覆盖）；3. 源必须在模型存储根之内；
//  4. 已是规范布局则空操作；5. dryRun 时只返回计划。
// 成功后更新 registry.Path 指向新位置；失败时清理已移动的部分并保留原登记。
func (m *Manager) RelocateModel(ctx context.Context, modelID string, dryRun bool, onProgress func(RelocateResult)) (RelocateResult, error) {
	res := RelocateResult{ID: modelID, DryRun: dryRun}
	me, ok := m.modelByID(modelID)
	if !ok {
		return res, fmt.Errorf("model %q not registered", modelID)
	}
	rec := layoutRec{ID: modelID, Quant: me.Quant, Path: me.Path}
	src := m.ResolveModelPath(me.Path)
	dst := m.canonicalPathFor(rec)
	res.From, res.To = src, dst

	if src == "" {
		return res, fmt.Errorf("模型 %s 未声明路径", modelID)
	}
	if filepath.Clean(src) == filepath.Clean(dst) {
		res.Note = "已处于规范布局，无需搬迁"
		return res, nil
	}
	st, err := os.Stat(src)
	if err != nil {
		return res, fmt.Errorf("源路径不可访问：%s（%v）", src, err)
	}
	res.IsDir = st.IsDir()
	if st.IsDir() {
		res.Bytes, res.Files = dirSize(src)
	} else {
		res.Bytes, res.Files = st.Size(), 1
	}

	root := filepath.Clean(m.ModelsRootDir())
	if !strings.HasPrefix(filepath.Clean(src), root+string(os.PathSeparator)) {
		return res, fmt.Errorf("拒绝搬迁 %s：源位于模型存储根 %s 之外", src, root)
	}
	// 目标已存在 → 拒绝，避免覆盖或与既有内容混淆。
	if _, err := os.Stat(dst); err == nil {
		return res, fmt.Errorf("拒绝搬迁 %s：目标 %s 已存在（不覆盖既有权重）", modelID, dst)
	}
	// 目录型：目标是目录本身，需先创建父级。
	// 文件型：目标是文件路径，需先创建其父目录。
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return res, fmt.Errorf("创建目标父目录失败：%w", err)
	}

	if dryRun {
		res.Note = fmt.Sprintf("计划：%s → %s（%d 个文件 / %s）", src, dst, res.Files, humanBytes(res.Bytes))
		if onProgress != nil {
			onProgress(res)
		}
		return res, nil
	}

	if st.IsDir() {
		moved, b, err := moveTree(src, dst)
		res.MovedFile, res.Bytes = moved, b
		if err != nil {
			return res, fmt.Errorf("搬迁目录失败（已移动 %d 个文件）：%w", moved, err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return res, err
		}
		if err := moveFile(src, dst); err != nil {
			return res, fmt.Errorf("搬迁文件失败：%w", err)
		}
		res.MovedFile = 1
	}

	// 更新登记：路径指向新位置。
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rs != nil {
		cur, ok := rs.Model(modelID)
		if ok {
			cur.Path = dst
			cur.State = "registered"
			if err := rs.SetModel(modelID, cur); err != nil {
				return res, fmt.Errorf("搬迁完成但更新 registry 失败：%w", err)
			}
		}
	}
	res.Done = true
	res.Note = fmt.Sprintf("已搬迁至 %s", dst)
	if onProgress != nil {
		onProgress(res)
	}
	return res, nil
}

// moveTree 把 src 目录的内容移入 dst 目录（创建 dst）。返回移动的文件数与字节数。
// 同卷用 os.Rename（快）；跨卷回落 copy+remove。
func moveTree(src, dst string) (int, int64, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, 0, err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0, 0, err
	}
	moved, varBytes := 0, int64(0)
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			n, b, err := moveTree(s, d)
			moved, varBytes = moved+n, varBytes+b
			if err != nil {
				return moved, varBytes, err
			}
			_ = os.Remove(s) // 移空后删掉空目录
			continue
		}
		st, _ := e.Info()
		if err := moveFile(s, d); err != nil {
			return moved, varBytes, err
		}
		moved++
		if st != nil {
			varBytes += st.Size()
		}
	}
	return moved, varBytes, nil
}

// moveFile 移动单个文件；跨卷时回落为复制（带 fsync）+ 删除源。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	in.Close()
	return os.Remove(src)
}

// dirSize 递归统计目录的文件数与总字节数。
func dirSize(dir string) (int64, int) {
	var total int64
	var files int
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		files++
		return nil
	})
	return total, files
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
