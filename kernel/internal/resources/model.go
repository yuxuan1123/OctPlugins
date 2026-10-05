package resources

import (
	"context"
	"time"
)

// kind=model：声明 + 宿主侧加载/卸载（转化域 §四 加载权硬约束）。
//
// 加载权归属（硬约束）：
//   - 只有宿主可加载/卸载模型，经 ResourceMap.Acquire/Release 由内核执行；
//   - 插件与 tool / outtool 均不得自行加载，只能请求宿主加载；
//   - tool / outtool 内部只做推理，不做权重拉取、不做显存调度。
//
// 因此 model 的生命周期动作是：宿主 Acquire → 经 ModelHooks.Ensure 指令 provider tool
// 把权重载入其进程（tool 只执行、不自行触发）→ 就绪登记 → Release 时经 ModelHooks.Release 卸载（不删盘）。

// ModelHandle 模型运行时句柄（§14.4 Handle 的 model 侧）。
type ModelHandle struct {
	ID         string
	Backend    string // 提供它的 tool id（backend 工具）
	Capability string // ocr / tts / stt / translate；可选项
	Quant      string
	Path       string // 模型根下的相对路径，如 <modelId>/<quant>/（§20.5）
	SHA256     string
	State      string // unregistered / loading / ready / failed
}

// ModelHooks 由生命周期层注入的宿主侧加载/卸载动作。
// Ensure 请求 provider tool 加载权重并等到就绪；Release 请求其释放运行时占用。
// 二者均由宿主在 Acquire/Release 路径上调用，tool 不得自行触发。
type ModelHooks struct {
	Ensure  func(ctx context.Context) error
	Release func() error
}

// NewModel 构造一个模型条目。backend 为提供它的 tool id；capability 用于 §10.1 的能力维度上限。
func NewModel(id, backend, capability, quant, path string) *Entry {
	h := &ModelHandle{
		ID: id, Backend: backend, Capability: capability,
		Quant: quant, Path: path, State: "unregistered",
	}
	e := &Entry{
		ID:         id,
		Kind:       KindModel,
		Backend:    backend,
		Capability: capability,
		Handle:     h,
		State:      "unregistered",
	}
	// 默认钩子：无 provider 可实现 model.ensure 时退化为「登记即就绪」，
	// 保持与旧行为兼容（provider 未实现加载指令时不让整链失败）。
	e.spawn = func(ctx context.Context) error {
		h.State = "ready"
		e.State = "ready"
		return nil
	}
	e.stop = func() error {
		// §14.7 unload：释放运行时占用，权重文件保留；MUST NOT 删盘。
		h.State = "unregistered"
		e.State = "unregistered"
		return nil
	}
	return e
}

// SetModelHooks 注入宿主侧加载/卸载动作（由生命周期层在登记 providesModels 时调用）。
// 传入 hooks.Ensure 后，Acquire 会真正指令 provider tool 加载权重并把状态推进到 ready。
func (e *Entry) SetModelHooks(h ModelHooks) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h.Ensure != nil {
		e.spawn = func(ctx context.Context) error {
			if mh, ok := e.Handle.(*ModelHandle); ok {
				mh.State = "loading"
			}
			e.State = "loading"
			if err := h.Ensure(ctx); err != nil {
				if mh, ok := e.Handle.(*ModelHandle); ok {
					mh.State = "failed"
				}
				e.State = "failed"
				return err
			}
			if mh, ok := e.Handle.(*ModelHandle); ok {
				mh.State = "ready"
			}
			e.State = "ready"
			return nil
		}
	}
	if h.Release != nil {
		e.stop = func() error {
			err := h.Release()
			if mh, ok := e.Handle.(*ModelHandle); ok {
				mh.State = "unregistered"
			}
			e.State = "unregistered"
			return err
		}
	}
}

// ModelMeta 返回模型的声明元信息（供状态视图/资源账使用）。
func (e *Entry) ModelMeta() (backend, capability, quant, path string, ok bool) {
	mh, isModel := e.Handle.(*ModelHandle)
	if !isModel {
		return "", "", "", "", false
	}
	return mh.Backend, mh.Capability, mh.Quant, mh.Path, true
}

// SetGraceUntil 设置/清除 handover 窗口截止时刻（§11.2）。
// 仅作外部可见的镜像字段；上限计算以 Manager.grace 为准（见 map.go 的锁序说明）。
func (e *Entry) SetGraceUntil(t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.GraceUntil = t
}

// GraceUntilAt 读取 handover 窗口截止时刻。
func (e *Entry) GraceUntilAt() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.GraceUntil
}
