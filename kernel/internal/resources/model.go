package resources

import "context"

// kind=model：声明 + 就绪登记（§14.11 进程内边界）。
//
// 一期宿主只负责资源文件的声明、定位、引用计数与状态登记，MUST NOT 承担加载与推理调度。
// 因此 model 的生命周期动作仅：拉取权重（校验 sha256，P3）→ 就绪登记 → 卸载（不删盘）。

// ModelHandle 模型运行时句柄（§14.4 Handle 的 model 侧）。
type ModelHandle struct {
	ID      string
	Backend string
	Quant   string
	Path    string // 相对项目根，如 models/<modelId>/<quant>/
	SHA256  string
	State   string // ModelState：unregistered/pulling/ready/unloading/failed
}

// NewModel 构造一个模型条目（就绪登记用）。
func NewModel(id, backend, quant, path string) *Entry {
	h := &ModelHandle{ID: id, Backend: backend, Quant: quant, Path: path, State: "unregistered"}
	e := &Entry{
		ID:      id,
		Kind:    KindModel,
		Backend: backend,
		Handle:  h,
		State:   "unregistered",
	}
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
