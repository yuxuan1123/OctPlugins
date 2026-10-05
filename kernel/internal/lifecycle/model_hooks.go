package lifecycle

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/octplugin/kernel/internal/resources"
	"github.com/octplugin/kernel/pkg/protocol"
)

// 本文件：模型加载权的宿主侧实现（转化域 §四 加载权硬约束）。
//
// 链路：conversion → 宿主（加载 tool / outtool）；tool → 宿主（加载 model）。
// 内核是唯一发起方：Acquire(model) 经 ModelHooks.Ensure 指令 provider tool 执行 "model.ensure"，
// Release(model) 指令 "model.release"。tool 内部只做推理，不自行加载权重、不做显存调度。
//
// 向后兼容：provider tool 尚未实现 model.ensure / model.release 时，
// 按 §14.11 旧的「登记即就绪」语义降级并记一条诊断日志，不让整链失败。

// modelEnsureTimeout 单次模型加载指令的默认上限。
// 声明里最大的 coldStartMs 是 15000（MOSS-TTS / hy_mt），留足冷启动与磁盘 IO 余量。
const modelEnsureTimeout = 90 * time.Second

// modelReleaseTimeout 卸载指令上限（释放运行时占用通常很快）。
const modelReleaseTimeout = 15 * time.Second

// modelHooksFor 构造某模型的宿主侧加载/卸载钩子，绑定到提供它的 tool。
func (m *Manager) modelHooksFor(modelID, providerID string) resources.ModelHooks {
	return resources.ModelHooks{
		Ensure: func(ctx context.Context) error {
			return m.callProviderModel(ctx, providerID, "model.ensure", modelID, modelEnsureTimeout)
		},
		Release: func() error {
			return m.callProviderModel(context.Background(), providerID, "model.release", modelID, modelReleaseTimeout)
		},
	}
}

// callProviderModel 经 §11.6 通道指令 provider tool 加载/卸载某模型。
//
// §1.4/§四：tool 只按 id 申请，**路径由宿主下发**——tool 不再自己硬编码权重位置。
// 这样 §20.5 的模型搬迁（<modelId>/<quant>/ 规范化）才不会让 tool 失联：
// 宿主改 registry.Path 后，下一次 model.ensure 就会把新路径传下去。
//
// 返回值语义：
//   - 成功（tool 应答 ok）→ nil；
//   - tool 未实现该方法（-32601）→ nil + 诊断日志（降级为登记即就绪，兼容未改造 tool）；
//   - tool 报告加载失败（权重缺失/OOM 等）→ 非 nil，调用方据此把模型标记为 failed。
func (m *Manager) callProviderModel(ctx context.Context, providerID, method, modelID string, defTimeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	pl, err := m.GetOrStart(providerID)
	if err != nil {
		return fmt.Errorf("model provider %s unavailable: %w", providerID, err)
	}
	if !pl.Alive() {
		return fmt.Errorf("model provider %s is down", providerID)
	}
	params := map[string]any{"modelId": modelID}
	if me, ok := m.modelByID(modelID); ok {
		if p := m.ResolveModelPath(me.Path); p != "" {
			params["path"] = p
		}
		if quant := me.Quant; quant != "" {
			params["quant"] = quant
		}
	}
	// 伴随模型（如 OCR 的 det↔rec、MOSS 的 codec）随主模型一起加载，
	// 故把同一 provider 下的伴随模型路径一并下发，避免 tool 再去猜。
	if comp := m.companionPathsFor(modelID); len(comp) > 0 {
		params["companionPaths"] = comp
	}
	timeout := defTimeout
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	resp, err := pl.Call(method, params, timeout)
	if err != nil {
		return fmt.Errorf("%s %s: %w", providerID, method, err)
	}
	if resp.Error != nil {
		if resp.Error.Code == protocol.ErrMethodNotFound {
			// 未改造的 tool：保持旧语义（宿主只做登记），但留下诊断痕迹。
			log.Printf("[models] provider %s does not implement %s; falling back to readiness-registration for %s",
				providerID, method, modelID)
			return nil
		}
		return fmt.Errorf("%s %s(modelId=%s): code=%d %v", providerID, method, modelID, resp.Error.Code, resp.Error.Data)
	}
	return nil
}

// companionPathsFor 返回与 modelID 同属一个「模型组」的伴随模型路径。
//
// 判定：同一个 provider、Companion=true、且 capability 与主模型一致。
// 例：rapidocr(det) ↔ rapidocr-rec(rec)；MOSS-TTS ↔ MOSS-Audio-Tokenizer。
func (m *Manager) companionPathsFor(modelID string) map[string]string {
	base, ok := m.modelByID(modelID)
	if !ok {
		return nil
	}
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rs == nil {
		return nil
	}
	f, err := rs.Load()
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for id, me := range f.Models {
		if !me.Companion || id == modelID {
			continue
		}
		if me.Provider != base.Provider {
			continue
		}
		if base.Capability != "" && me.Capability != base.Capability {
			continue
		}
		if p := m.ResolveModelPath(me.Path); p != "" {
			out[id] = p
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
