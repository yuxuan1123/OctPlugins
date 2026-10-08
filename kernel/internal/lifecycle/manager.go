package lifecycle

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/octplugin/kernel/internal/perms"
	"github.com/octplugin/kernel/internal/registry"
	"github.com/octplugin/kernel/internal/resources"
	"github.com/octplugin/kernel/internal/watchdog"
	"github.com/octplugin/kernel/pkg/protocol"
)

// Fn 一条已注册的共享函数（FR-8 注册表条目）。
type Fn struct {
	Name     string
	Method   string
	Desc     string
	PluginID string
}

// RegState 插件生命周期状态（Phase 1/2 状态机：registered→running→idle→stopped）。
type RegState int

const (
	RegRegistered RegState = iota // 已注册，未启动（lazy/prewarm 或等待）
	RegStarting                   // 正在启动（GetOrStart 并发等待）
	RegRunning                    // 运行中（健康）
	RegIdle                       // 已空闲回收，可随时按需重启
	RegStopped                    // 停止（禁用/手动停/退避超限/正常退出）
)

func (s RegState) String() string {
	switch s {
	case RegRegistered:
		return "REGISTERED"
	case RegStarting:
		return "STARTING"
	case RegRunning:
		return "RUNNING"
	case RegIdle:
		return "IDLE"
	default:
		return "STOPPED"
	}
}

// recycleCheckInterval 空闲回收扫描周期（Phase 1 约定 10s）。
const recycleCheckInterval = 10 * time.Second

// §12.2.2 / §10.1.2：状态机单 goroutine —— 所有生命周期状态（r.state）改写都必须是
// 事件，经 stEvCh channel 汇入唯一的 stateLoop goroutine，禁止多 goroutine 直接改写。
// stOp 区分事件类型，stEvent 携带一次迁移所需数据；stResult 承载同步调用方（RPC）的返回。
type stOp string

const (
	opStart   stOp = "start"   // 请求启动/幂等返回运行实例（GetOrStart/startAlways/Restart/timer/Import）
	opExit    stOp = "exit"    // 进程退出/崩溃（onProcessExit/healthSweep 上报）
	opRecycle stOp = "recycle" // 空闲回收（recallSweep）
	opStop    stOp = "stop"    // 停止并可选移除登记（StopPlugin/SetLifecycle-disabled）
	opStopAll stOp = "stopall" // 内核退出统一停止（StopAll）
	opRestart stOp = "restart" // 停止后立即重启（Restart/SetLifecycle 变更策略）
)

type stEvent struct {
	op     stOp
	id     string
	p      *Plugin // opExit/opRecycle 涉及的运行实例
	code   int     // opExit 的退出码
	remove bool    // opStop：停止后同时注销共享函数并删除登记项（StopPlugin）
	resp   chan stResult
}

type stResult struct {
	p   *Plugin
	err error
}

func (op stOp) String() string { return string(op) }

// reg 一个插件的生命周期登记项。
type reg struct {
	id string
	mf Manifest

	disabled    bool // load_mode == disabled
	depsPending bool // 依赖未就绪（如 Node 无解释器），暂不可启动
	installing  bool // §6.4：依赖正在后台安装（防同一插件重复触发安装）

	mu      sync.Mutex
	state   RegState
	running *Plugin
	crash   []time.Time // 滑动窗口内崩溃时间戳（退避重启依据）
}

type Manager struct {
	mu            sync.Mutex
	pluginsDir    string
	storeDir      string
	regStore      *registry.Store // §8：registry.json 唯一写入方（app 注入后启用）
	regs          map[string]*reg
	overrides     overridesFile // 用户覆盖 map[pluginID]Override（持久化 state/overrides.json）
	pythonPath    string
	gate          *perms.Gate
	venvPython    func(Manifest) string
	depsReady     func(Manifest) bool // Python 依赖就绪判定（RequirementsSatisfied）；nil 时默认就绪
	nodeBin       func(Manifest) (string, bool)
	sdkPyPath     string                                                            // §16.5：oct_sdk 根（root/sdk/python），spawn 经 PYTHONPATH 注入
	depsPlan      func(Manifest) (jsonHash, lockHash, cacheDir string, err error)   // §6/§8：spawn 前锁判定并入 registry
	install       func(Manifest, func(phase string)) error                          // §6.4：真实依赖安装（Installer.Install）+ 进度回调
	onSpawn       func(*Plugin)                                                     // 注入 Event 广播出口（内核装配）
	onState       func(id, state string)                                            // 进程状态变更回调（内核装配→宿主刷新 UI）
	onDepsProg    func(id, phase string)                                            // §6.4：依赖安装进度回调（内核装配→宿主广播）
	onModelEvt    func(typ string, data any)                                        // 转化域 §12.5：模型 handover 事件（内核装配→宿主广播）
	pinsFn        func() map[string]string                                          // 转化域 §8.1：用户能力级 pin 读取器（装配根注入）
	pinWriteFn    func(capability, modelID string) error                            // 转化域 §8.1：pin 写入器（装配根注入，落 user-settings.json）
	settingsGetFn func(pluginID string, mf Manifest) (map[string]any, error)        // §17.2 A：插件读自己的生效设置（装配根注入）
	settingsSetFn func(pluginID string, mf Manifest, settings map[string]any) error // §17.2 A：插件写自己的设置（装配根注入）
	tokenGen      func() string                                                     // §11.4：每次 spawn 一次性生成握手 token（默认 crypto/rand hex）

	resources  *resources.Manager // §14 ResourceMap（extern deps 登记/获取/释放）
	toolsRoot  string             // §14.2 tools/ 根（bundled 定位用）
	modelsRoot string             // 转化域 §20.5：模型存储根（空 = DefaultModelsRoot）

	patrolOnce sync.Once // §12.2.3：健康巡检单 goroutine（全插件共享），once 防重复启动

	stOnce sync.Once     // §12.2.2 状态机单 goroutine（once 防重复启动）
	stEvCh chan *stEvent // §12.2.2 状态迁移事件汇入唯一 state-loop goroutine

	fnsMu sync.Mutex
	fns   map[string]Fn
}

func NewManager(pluginsDir string, gate *perms.Gate) *Manager {
	return &Manager{
		pluginsDir: pluginsDir,
		regs:       make(map[string]*reg),
		fns:        make(map[string]Fn),
		gate:       gate,
		stEvCh:     make(chan *stEvent, 128),
	}
}

// SetVenvResolver 注入依赖隔离的解释器解析器（deps.Installer.VenvPython）。
func (m *Manager) SetVenvResolver(fn func(Manifest) string) {
	m.mu.Lock()
	m.venvPython = fn
	m.mu.Unlock()
}

// SetDepsReadyResolver 注入 Python 依赖就绪判定（deps.Installer.RequirementsSatisfied）。
// 未注入视为无条件就绪（等价于旧行为）。
func (m *Manager) SetDepsReadyResolver(fn func(Manifest) bool) {
	m.mu.Lock()
	m.depsReady = fn
	m.mu.Unlock()
}

// SetNodeResolver 设置 Node 插件解释器解析器。
func (m *Manager) SetNodeResolver(fn func(Manifest) (string, bool)) {
	m.mu.Lock()
	m.nodeBin = fn
	m.mu.Unlock()
}

// SetSdkPythonPath 注入 oct_sdk 根（§16.5：root/sdk/python），spawn 时经 PYTHONPATH 注入插件。
func (m *Manager) SetSdkPythonPath(p string) {
	m.mu.Lock()
	m.sdkPyPath = p
	m.mu.Unlock()
}

// SetDepsPlan 注入 §6/§8 依赖锁计划解析器（runtime.Installer.DepsPlan），
// spawn 前求 jsonHash/lockHash/cacheDir 并写入 registry 条目。未注入则不记录。
func (m *Manager) SetDepsPlan(fn func(Manifest) (jsonHash, lockHash, cacheDir string, err error)) {
	m.mu.Lock()
	m.depsPlan = fn
	m.mu.Unlock()
}

// SetInstaller 注入 §6.4 真实依赖安装函数（runtime.Installer.Install）。
// 依赖未命中时：置 depsState=preparing → 后台调用此函数安装 → 成功后自动续启插件。
// 第二参数为安装进度回调（§6.4 MUST：向 UI 广播安装阶段）。
func (m *Manager) SetInstaller(fn func(Manifest, func(phase string)) error) {
	m.mu.Lock()
	m.install = fn
	m.mu.Unlock()
}

// SetOnDepsProgress 注入依赖安装进度回调（内核装配→宿主广播 plugin.deps.progress，§6.4 MUST）。
func (m *Manager) SetOnDepsProgress(fn func(id, phase string)) {
	m.mu.Lock()
	m.onDepsProg = fn
	m.mu.Unlock()
}

// SetOnModelEvent 注入模型 handover 事件回调（转化域 §12.5：全程对用户可见）。
// 内核装配时接到 ws 广播（source=kernel，type=models.handover.*）。
func (m *Manager) SetOnModelEvent(fn func(typ string, data any)) {
	m.mu.Lock()
	m.onModelEvt = fn
	m.mu.Unlock()
}

// emitModelEvent 向宿主广播一条模型事件；未注入回调时静默丢弃（不影响编排）。
func (m *Manager) emitModelEvent(typ string, data any) {
	m.mu.Lock()
	fn := m.onModelEvt
	m.mu.Unlock()
	if fn != nil {
		fn(typ, data)
	}
}

// SetOnSpawn 设置插件启动后的钩子（内核用它注入 Event 广播出口）。
func (m *Manager) SetOnSpawn(fn func(*Plugin)) {
	m.mu.Lock()
	m.onSpawn = fn
	m.mu.Unlock()
}

// SetOnState 注入进程状态变更回调（内核装配→宿主实时刷新侧栏圆点）。
func (m *Manager) SetOnState(fn func(id, state string)) {
	m.mu.Lock()
	m.onState = fn
	m.mu.Unlock()
}

// SetTokenGenerator 注入 §11.4 握手 token 生成器（默认 crypto/rand hex，无需显式设置）。
func (m *Manager) SetTokenGenerator(fn func() string) {
	m.mu.Lock()
	m.tokenGen = fn
	m.mu.Unlock()
}

// newToken 生成一次性握手 token（§11.4：crypto/rand，仅经环境变量传递，不落盘）。
func (m *Manager) newToken() string {
	m.mu.Lock()
	gen := m.tokenGen
	m.mu.Unlock()
	if gen != nil {
		return gen()
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("tok%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// emitState 触发状态变更回调（异步，避免持有锁时回调卡住启动路径）。
func (m *Manager) emitState(r *reg, state string) {
	m.mu.Lock()
	fn := m.onState
	m.mu.Unlock()
	if fn != nil {
		go fn(r.id, state)
	}
}

func (m *Manager) applySpawnWire(p *Plugin) {
	m.mu.Lock()
	fn := m.onSpawn
	m.mu.Unlock()
	if fn != nil {
		fn(p)
	}
}

// interp 按插件类型解析解释器路径；第二返回值 true 表示 Node 插件。
// Go 插件：返回二进制绝对路径作为「解释器」，Start 据此直接执行（不再拼接 entry）。
func (m *Manager) interp(mf Manifest) (string, bool) {
	// Go 插件：entry 即编译后的二进制，直接执行，无解释器。
	if isGoType(mf.Type) {
		return filepath.Join(mf.Dir, mf.Entry), false
	}

	// 读取快照（可在启动时安全调用）
	m.mu.Lock()
	nodeBin, venvPython := m.nodeBin, m.venvPython
	pythonPath := m.pythonPath
	m.mu.Unlock()

	if nodeBin != nil {
		if b, ok := nodeBin(mf); ok {
			return b, true
		}
	} else if isNodeType(mf.Type) {
		return "", true // 已声明 Node 但未注册解析器：暴露缺解释器
	}
	py := pythonPath
	if venvPython != nil {
		if vp := venvPython(mf); vp != "" {
			py = vp
		}
	}
	return py, false
}

func isNodeType(t string) bool {
	lt := strings.ToLower(t)
	return strings.Contains(lt, "node") || strings.Contains(lt, "js") || strings.Contains(lt, "javascript")
}

// isGoType 判断是否为 Go 编译型插件（entry 为可直接执行的二进制）。
func isGoType(t string) bool {
	lt := strings.ToLower(strings.TrimSpace(t))
	return lt == "go" || lt == "golang"
}

// pluginReady 判断插件是否可启动（依赖是否就绪）。
// Node：需 node 解释器可用 且 node_modules 依赖就绪（depsReady）才就绪，
//
//	否则走 §6.4 后台安装 gating（package.json 有依赖但未装 node_modules → 触发 pnpm 安装）；
//
// Python：需已注入的就绪判定通过（默认通过，等价旧行为）。
func (m *Manager) pluginReady(mf Manifest) bool {
	// Go 编译型插件：二进制已随包分发，无解释器/依赖安装步骤，直接就绪。
	if isGoType(mf.Type) {
		return true
	}
	isNode := isNodeType(mf.Type)
	m.mu.Lock()
	nodeBin := m.nodeBin
	dr := m.depsReady
	m.mu.Unlock()
	if isNode {
		if nodeBin == nil {
			return false
		}
		if _, ok := nodeBin(mf); !ok {
			return false
		}
		if dr != nil {
			return dr(mf) // node_modules 依赖是否就绪
		}
		return true
	}
	if dr == nil {
		return true
	}
	return dr(mf)
}

// ErrDepsInstalling 表示插件依赖未就绪、已进入后台安装态（§6.4）。
// 调用方应视作「插件暂不可用，安装完成后会自动启动」，而非硬失败。
var ErrDepsInstalling = errors.New("dependencies installing")

// setDepsState 读-改-写 registry 条目的 depsState 字段（§8 唯一写入方串行化）。
// 保留其余字段（version / hash / cacheDir / dataDir / manifestHash）。
func (m *Manager) setDepsState(id, state string) {
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rs == nil {
		return
	}
	entry, ok := rs.Plugin(id)
	if !ok {
		return
	}
	entry.DepsState = state
	_ = rs.SetPlugin(id, entry)
}

// markInstalling / clearInstalling 保护同一插件不被并发重复触发安装（§6.4）。
func (r *reg) markInstalling() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.installing {
		return false
	}
	r.installing = true
	return true
}

func (r *reg) clearInstalling() {
	r.mu.Lock()
	r.installing = false
	r.mu.Unlock()
}

// doInstall 在后台执行真实依赖安装（§6.4）。未注入安装器视为未配置（上报而非静默）。
// 安装阶段经 onDepsProg 广播给宿主（§6.4 MUST：安装过程向 UI 广播进度）。
func (m *Manager) doInstall(mf Manifest) error {
	m.mu.Lock()
	fn := m.install
	m.mu.Unlock()
	if fn == nil {
		return errors.New("dependency installer not configured")
	}
	return fn(mf, func(phase string) { m.emitDepsProgress(mf.ID, phase) })
}

// emitDepsProgress 广播依赖安装进度阶段（异步，避免安装期间持有锁）。
func (m *Manager) emitDepsProgress(id, phase string) {
	m.mu.Lock()
	fn := m.onDepsProg
	m.mu.Unlock()
	if fn != nil {
		go fn(id, phase)
	}
}

// triggerDepsInstall §6.4 依赖后台安装触发（幂等：同一插件已有安装在途时不重复触发）。
// 返回 true 表示本次调用新启动了安装。安装完成后由 depsInstallThenStart 自动续启。
func (m *Manager) triggerDepsInstall(r *reg) bool {
	if !r.markInstalling() {
		return false
	}
	m.setDepsState(r.id, "preparing")
	m.emitState(r, "PREPARING")
	go m.depsInstallThenStart(r)
	return true
}

// depsInstallThenStart §6.4 后台安装完成后的续启：成功 → depsState=ready 并重新触发启动；
// 失败 → depsState=error，插件置 depsPending（留待用户处理），并向 UI 广播状态。
func (m *Manager) depsInstallThenStart(r *reg) {
	defer r.clearInstalling()
	if err := m.doInstall(r.mf); err != nil {
		r.mu.Lock()
		r.depsPending = true
		r.mu.Unlock()
		r.mu.Lock()
		r.state = RegStopped
		r.mu.Unlock()
		m.setDepsState(r.id, "error")
		m.emitState(r, "DEPENDENCY_ERROR")
		log.Printf("[kernel] deps install %s failed: %v", r.id, err)
		return
	}
	m.setDepsState(r.id, "ready")
	log.Printf("[kernel] deps installed %s; continuing start", r.id)
	if _, err := m.startNow(r); err != nil {
		log.Printf("[kernel] start after deps %s: %v", r.id, err)
	}
}

// Discover 扫描 plugins/<id>/manifest.json，返回清单（不含实例）。
func (m *Manager) Discover() ([]Manifest, error) {
	entries, err := os.ReadDir(m.pluginsDir)
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		id := e.Name()
		mf, err := readManifest(m.pluginsDir, id)
		if err != nil {
			log.Printf("[scan] %s: %v", id, err)
			continue
		}
		out = append(out, mf)
	}
	return out, nil
}

// decodeManifest 严格解析 manifest：§7.5 要求 DisallowUnknownFields ——
// 作者写了宿主不支持的字段 MUST 报错而非静默忽略，以便尽早暴露 schema 失配。
func decodeManifest(b []byte) (Manifest, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var mf Manifest
	if err := dec.Decode(&mf); err != nil {
		return mf, err
	}
	return mf, nil
}

func readManifest(dir, id string) (Manifest, error) {
	paths := []string{
		filepath.Join(dir, id, "manifest.json"),
		filepath.Join(dir, id, "plugin.json"),
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err == nil {
			mf, err := decodeManifest(b)
			if err != nil {
				return mf, fmt.Errorf("manifest parse: %w", err)
			}
			mf.ID = id
			mf.Dir = filepath.Join(dir, id)
			return mf, normalizeManifest(&mf)
		}
	}
	return Manifest{}, os.ErrNotExist
}

func readManifestInDir(dir string) (Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Manifest{}, err
	}
	mf, err := decodeManifest(b)
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest parse: %w", err)
	}
	mf.ID = strings.TrimSpace(mf.ID)
	mf.Dir = dir
	return mf, normalizeManifest(&mf)
}

// normalizeManifest 对已解析的 manifest 应用默认值并校验：旧清单缺失新字段自动回填；非法取值上抛跳过。
func normalizeManifest(mf *Manifest) error {
	mf.LifecyclePolicy.ApplyDefaults()
	if err := mf.LifecyclePolicy.Validate(); err != nil {
		return fmt.Errorf("manifest %s lifecycle invalid: %w", mf.ID, err)
	}
	return nil
}

func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s not a directory", src)
	}
	return copyDirRec(src, dst)
}

func copyDirRec(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if err := copyDirRec(s, d); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// recordDeps 在 spawn 前求 §6 依赖锁计划并把 jsonHash/lockHash/cacheDir/depsState
// 写入 registry.json（§8）。依赖缺失时把依赖解析失败作为错误返回（§10.2 ③ 前置门槛）。
func (m *Manager) recordDeps(mf Manifest) error {
	m.mu.Lock()
	fn, rs, storeDir := m.depsPlan, m.regStore, m.storeDir
	m.mu.Unlock()
	if fn == nil || rs == nil {
		return nil // 未装配依赖计划/registry：不记录（沿用旧行为）
	}
	dataDir := ""
	if storeDir != "" {
		dataDir = filepath.Join(storeDir, "plugins", mf.ID)
	}
	jsonHash, lockHash, cacheDir, err := fn(mf)
	if err != nil {
		return fmt.Errorf("deps plan %s: %w", mf.ID, err)
	}
	// §6.4：depsState 必须如实反映依赖就绪与否。就绪 → "ready"；未就绪 → "missing"，
	// 由 loopSpawn 的 gating 在触发后台安装时升级为 "preparing"（而非恒为 ready 谎报）。
	ready := m.pluginReady(mf)
	depsState := "missing"
	if ready {
		depsState = "ready"
	}
	// 现有条目 + 依赖计划字段合并（保留 version/path/manifestHash/dataDir）。
	entry := registry.Entry{
		Version:   mf.Version,
		Path:      filepath.Join("plugins", mf.ID),
		JSONHash:  jsonHash,
		LockHash:  lockHash,
		CacheDir:  cacheDir,
		DepsState: depsState,
		DataDir:   dataDir,
	}
	if prev, ok := rs.Plugin(mf.ID); ok {
		entry.ManifestHash = prev.ManifestHash
		if entry.DataDir == "" {
			entry.DataDir = prev.DataDir
		}
	}
	return rs.SetPlugin(mf.ID, entry)
}

// syncRegistryEntry 把插件已安装事实登记到 registry.json（§8 唯一写入方）。
// dataDir 仅在 storeDir 已设置（未命中的场景用于单测跳过）时写入。
// §20.5：登记即记录 manifestHash（manifest 原始文件的 sha256），供 spawn 前篡改校验。
func (m *Manager) syncRegistryEntry(id, version string, mf Manifest) {
	m.mu.Lock()
	rs := m.regStore
	storeDir := m.storeDir
	pluginsDir := m.pluginsDir
	m.mu.Unlock()
	if rs == nil {
		return
	}
	dataDir := ""
	if storeDir != "" {
		dataDir = filepath.Join(storeDir, "plugins", id)
	}
	mh, _ := manifestFileHash(filepath.Join(pluginsDir, id))
	entry := registry.Entry{
		Version:      version,
		Path:         filepath.Join("plugins", id),
		ManifestHash: mh,
		DataDir:      dataDir,
		DepsState:    "ready",
	}
	// §20.5：篡改校验基准只应「首次建立」，不得每次 boot 重钉——否则攻击者在 manifest
	// 被篡改后重启内核，篡改后的 hash 会取代遗漏前所用的基线，使检测失效。
	// 已登记过（含之前 boot 存的真实基线）时，保留旧 hash；仅首次登记才写入当前盘上 hash。
	if prev, ok := rs.Plugin(id); ok && prev.ManifestHash != "" {
		entry.ManifestHash = prev.ManifestHash
	}
	_ = rs.SetPlugin(id, entry)
}

// manifestFileHash 计算插件 manifest 原始文件内容（干净字节）的 sha256（§20.5 篡改校验基准）。
// 目录内优先 manifest.json，其次 plugin.json。
func manifestFileHash(dir string) (string, error) {
	for _, name := range []string{"manifest.json", "plugin.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			h := sha256.Sum256(b)
			return hex.EncodeToString(h[:]), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}
	return "", os.ErrNotExist
}

// tamperCheck §20.5：spawn 前重算 manifest 原始文件 hash 并与 registry.json 当前条目比对。
// 不符视为被篡改，拒绝启动（返回错误，oopSpawn 前置门槛）。未装配 registry 或首次无条目时不拦截。
func (m *Manager) tamperCheck(mf Manifest) error {
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rs == nil {
		return nil
	}
	cur, err := manifestFileHash(mf.Dir)
	if err != nil {
		return fmt.Errorf("recanon manifest: %w", err)
	}
	prev, ok := rs.Plugin(mf.ID)
	if !ok || prev.ManifestHash == "" {
		return nil // 无已登记基准：本次作为首次安装基线，信赖 manifest
	}
	if prev.ManifestHash != cur {
		return fmt.Errorf("manifest tampered (registry=%s on-disk=%s)", prev.ManifestHash, cur)
	}
	return nil
}

// register 以 manifest 创建登记项并声明权限。返回已有登记（幂等）。
func (m *Manager) register(mf Manifest) *reg {
	// 转化域 §23：manifest 契约校验（spawn 白名单 / net 与权限一致性）。
	// 自相矛盾的声明 MUST 拒绝登记，而不是留到运行期才暴露。
	if err := validateManifestContract(mf); err != nil {
		log.Printf("[scan] %s: %v", mf.ID, err)
		return nil
	}
	// 合并用户覆盖（override 优先于 manifest 默认值）
	mf.LifecyclePolicy = m.OverrideFor(mf.ID).apply(mf.LifecyclePolicy)
	if m.gate != nil {
		m.gate.Declare(mf.ID, mf.Permissions)
	}
	local := &reg{
		id:       mf.ID,
		mf:       mf,
		disabled: mf.LoadMode == LoadModeDisabled,
	}
	// 注意：登记阶段不探测依赖（pluginReady 可能同步 import，慢），只做快速扫描，
	// 保证 plugin.list / 侧栏「注册表预显示」立即可用。依赖就绪在真正启动时判定（startAlways / GetOrStart）。
	m.mu.Lock()
	if old, ok := m.regs[mf.ID]; ok {
		m.mu.Unlock()
		return old
	}
	m.regs[mf.ID] = local
	m.mu.Unlock()
	m.registerResourceDeps(mf)                 // §14：externalDependencies → ResourceMap 登记
	m.syncRegistryEntry(mf.ID, mf.Version, mf) // §8：登记即记入 registry.json（唯一写入方）
	m.registerProvidedModels(mf)               // 转化域 §三：providesModels → registry.json.models
	m.RegisterFunctions(mf)                    // §11.6：扫描期即注册共享函数，lazy tool 亦可被 registry.call 唤起（GetOrStart 兜底）
	return local
}

func (m *Manager) getReg(id string) *reg {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.regs[id]
}

// RegisterAll 扫描并快速登记全部插件（不探测依赖），供 plugin.list / 侧栏预显示。
// 在打印 auth 前同步调用，保证宿主首屏即拿到完整插件清单。
func (m *Manager) RegisterAll() {
	manifests, err := m.Discover()
	if err != nil {
		log.Printf("[kernel] discover: %v", err)
		return
	}
	for _, mf := range manifests {
		m.register(mf)
	}
}

// StartAll 启动常驻（load_mode=always）插件。每个插件在独立 goroutine 中
// 先做依赖就绪判定（带超时）再启动——一个插件依赖卡住只影响它自己，
// 不会阻塞其它插件或整体启动。
func (m *Manager) StartAll() {
	if py, err := LocatePython("3.12"); err != nil {
		log.Printf("[kernel] %v", err)
	} else {
		m.mu.Lock()
		m.pythonPath = py
		m.mu.Unlock()
	}
	for _, r := range m.snapshotRegs() {
		if r.disabled || r.mf.LoadMode != LoadModeAlways {
			continue
		}
		r := r
		go m.startAlways(r)
	}
}

// startAlways 启动单个常驻插件。依赖就绪与否统一由状态机 loopSpawn 的 §6.4 gating 判定：
// 未就绪 → 后台安装 → 完成后自动续启；失败 → DEPENDENCY_ERROR。此处不重复前置判定。
func (m *Manager) startAlways(r *reg) {
	if _, err := m.startNow(r); err != nil {
		log.Printf("[kernel] start always %s: %v", r.id, err)
	}
}

func (m *Manager) snapshotRegs() []*reg {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*reg, 0, len(m.regs))
	for _, r := range m.regs {
		out = append(out, r)
	}
	return out
}

// Plugin 返回当前运行中的实例（未运行返回 nil）。
func (m *Manager) Plugin(id string) *Plugin {
	r := m.getReg(id)
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != RegRunning {
		return nil
	}
	return r.running
}

// State 返回插件生命周期状态字符串。
func (m *Manager) State(id string) string {
	r := m.getReg(id)
	if r == nil {
		return "UNKNOWN"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.String()
}

// LifecycleView 返回某插件的生命周期策略视图：默认值(manifest) / 用户覆盖 / 最终生效。
type LifecycleView struct {
	PluginID  string          `json:"pluginId"`
	Defaults  LifecyclePolicy `json:"defaults"`  // manifest 声明值（未覆盖）
	Override  Override        `json:"override"`  // 用户覆盖（nil 字段=未覆盖）
	Effective LifecyclePolicy `json:"effective"` // 最终生效（default+override）
	State     string          `json:"state"`
}

// Lifecycle 返回某插件的生命周期策略信息（供设置 UI 渲染）。
func (m *Manager) Lifecycle(id string) LifecycleView {
	r := m.getReg(id)
	if r == nil {
		return LifecycleView{PluginID: id}
	}
	m.mu.Lock()
	ov := m.overrides[id]
	m.mu.Unlock()
	// defaults：重读 manifest 原文得到未覆盖前的策略默认值。
	mf, err := readManifest(m.pluginsDir, id)
	if err != nil {
		mf = r.mf
		mf.LifecyclePolicy = r.mf.LifecyclePolicy
	}
	effective := ov.apply(mf.LifecyclePolicy)
	effective.ApplyDefaults()
	return LifecycleView{
		PluginID:  id,
		Defaults:  mf.LifecyclePolicy,
		Override:  ov,
		Effective: effective,
		State:     m.State(id),
	}
}

// SetLifecycle 保存某插件的用户覆盖并立即生效：持久化后按新策略重启该插件。
func (m *Manager) SetLifecycle(id string, ov Override) error {
	if err := ValidateOverride(ov); err != nil {
		return err
	}
	if err := m.SaveOverride(id, ov); err != nil {
		return err
	}
	// 立即应用：刷新登记项里的策略（load_mode 变化需要重新评估是否常驻）。
	r := m.getReg(id)
	if r != nil {
		var mf Manifest
		rmf, err := readManifest(m.pluginsDir, id)
		if err == nil {
			mf = rmf
		} else {
			r.mu.Lock()
			mf = r.mf
			r.mu.Unlock()
		}
		mf.LifecyclePolicy = ov.apply(mf.LifecyclePolicy)
		r.mu.Lock()
		r.mf = mf
		r.disabled = mf.LoadMode == LoadModeDisabled
		r.mu.Unlock()
		// 重启以应用新资源/心跳/回收策略；disabled 则仅停止运行实例但保留登记。
		if r.disabled {
			m.post(&stEvent{op: opStop, id: id}) // 停止迁移交由状态机单 goroutine
			return nil
		}
		_ = m.Restart(id)
	}
	return nil
}

// GetOrStart 按需启动（懒启动核心）。running→直接返回；starting→等待；idle/stopped→重新 spawn。
// §6.4：依赖未就绪时不硬失败，而是触发后台安装并返回 ErrDepsInstalling（ring/call 路径同样
// 能自愈 —— 精简分发包首启用例：venv 未随包分发，首次调用即自动联网安装，装完自动续启）。
func (m *Manager) GetOrStart(id string) (*Plugin, error) {
	r := m.getReg(id)
	if r == nil {
		return nil, fmt.Errorf("unknown plugin %q", id)
	}
	if err := m.beginIfNeeded(r); err != nil {
		return nil, err
	}
	// 登记阶段未探测依赖，这里按需判定（带超时），保持 lazy/prewarm 的就绪门槛不变。
	if !m.pluginReady(r.mf) {
		m.triggerDepsInstall(r)
		return nil, ErrDepsInstalling
	}
	return m.startNow(r)
}

// beginIfNeeded 在启动前置检查：禁用/依赖未就绪直接返回错误。
func (m *Manager) beginIfNeeded(r *reg) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disabled {
		return fmt.Errorf("plugin %s is disabled", r.id)
	}
	if r.depsPending {
		return fmt.Errorf("plugin %s dependencies not installed", r.id)
	}
	return nil
}

// ── §12.2.2 状态机单 goroutine ────────────────────────────────
// 唯一的状态改写入口。所有请求（启动/退出/回收/停止/重启/全停）都转化为 stEvent
// 投递到 stEvCh，由 stateLoop 串行消费。任何 goroutine 都不得直接写 r.state。

// ensureStateLoop 确保状态机 goroutine 已启动（once）。首次有事件产生时延迟启动。
func (m *Manager) ensureStateLoop() {
	m.stOnce.Do(func() { go m.stateLoop() })
}

// stateLoop §12.2.2 状态机单 goroutine：串行消费全部生命周期事件。
func (m *Manager) stateLoop() {
	for ev := range m.stEvCh {
		m.applyEvent(ev)
	}
}

// post 投递一个需要同步结果的事件并等待状态机应答。
func (m *Manager) post(ev *stEvent) stResult {
	if ev.resp == nil {
		ev.resp = make(chan stResult, 1)
	}
	m.ensureStateLoop()
	m.stEvCh <- ev
	select {
	case res := <-ev.resp:
		return res
	case <-time.After(30 * time.Second):
		return stResult{err: fmt.Errorf("state op %s %s timed out", ev.op, ev.id)}
	}
}

// fire 投递一个只触发、不等待结果的事件（进程退出上报 / 空闲回收）。
func (m *Manager) fire(ev *stEvent) {
	m.ensureStateLoop()
	m.stEvCh <- ev
}

func (m *Manager) reply(ev *stEvent, res stResult) {
	if ev.resp != nil {
		ev.resp <- res
	}
}

// applyEvent 状态机单 goroutine 内部分发单个事件到对应迁移处理器。
func (m *Manager) applyEvent(ev *stEvent) {
	switch ev.op {
	case opStart:
		r := m.getReg(ev.id)
		if r == nil {
			m.reply(ev, stResult{err: fmt.Errorf("unknown plugin %q", ev.id)})
			return
		}
		p, err := m.loopStart(r)
		m.reply(ev, stResult{p: p, err: err})
	case opExit:
		if r := m.getReg(ev.id); r != nil {
			m.loopExit(r, ev.p, ev.code)
		}
	case opRecycle:
		if r := m.getReg(ev.id); r != nil {
			m.loopRecycle(r, ev.p)
		}
	case opStop:
		m.reply(ev, stResult{err: m.loopStop(ev)})
	case opRestart:
		p, err := m.loopRestart(ev)
		m.reply(ev, stResult{p: p, err: err})
	case opStopAll:
		m.loopStopAll()
		m.reply(ev, stResult{})
	}
}

// startNow 请求启动一次插件（幂等；并发安全）。真正迁移由状态机单 goroutine 执行。
func (m *Manager) startNow(r *reg) (*Plugin, error) {
	ev := &stEvent{op: opStart, id: r.id}
	res := m.post(ev)
	return res.p, res.err
}

// loopStart 状态机内启动迁移：running→直接返回；未运行→starting→spawn→running/stopped。
func (m *Manager) loopStart(r *reg) (*Plugin, error) {
	r.mu.Lock()
	if r.state == RegRunning {
		p := r.running
		r.mu.Unlock()
		return p, nil
	}
	if r.disabled {
		r.mu.Unlock()
		return nil, fmt.Errorf("plugin %s is disabled", r.id)
	}
	if r.depsPending {
		r.mu.Unlock()
		return nil, fmt.Errorf("plugin %s dependencies not installed", r.id)
	}
	r.state = RegStarting
	r.mu.Unlock()
	m.emitState(r, "STARTING")
	return m.loopSpawn(r)
}

// resolveEntry 按 §7.1 profile 决议入口：优先 manifest.defaults.profile 命中的 profile.entry，
// 未声明 profiles 时回落顶层 entry；defaults.profile 缺失/未命中时取首个 profile。
func (m *Manager) resolveEntry(mf Manifest) string {
	if len(mf.Profiles) == 0 {
		return mf.Entry
	}
	name := mf.Defaults.Profile
	if name == "" {
		for k := range mf.Profiles {
			name = k
			break
		}
	}
	if p, ok := mf.Profiles[name]; ok && p.Entry != "" {
		return p.Entry
	}
	return mf.Entry
}

// loopSpawn 状态机内派生一次插件进程（仅由 loopStart / loopRestart 调用，属于 stateLoop
// 单 goroutine）。成功置 running，失败置 stopped。
func (m *Manager) loopSpawn(r *reg) (*Plugin, error) {
	interp, _ := m.interp(r.mf)
	// §10.2 ①：查生效 profile → 用其 entry 派生（默认 profile 由 manifest.defaults 决议）。
	r.mf.Entry = m.resolveEntry(r.mf)
	if err := m.tamperCheck(r.mf); err != nil {
		return nil, fmt.Errorf("tamper check %s: %w", r.id, err)
	}
	// 转化域 §20.4：原生共享库校验（lockHash 锁不住 .dll/.so；不匹配拒绝派生进程）。
	if err := verifyNativeLibs(r.mf); err != nil {
		return nil, fmt.Errorf("native libs %s: %w", r.id, err)
	}
	// §10.2 ③ EnsureDeps：求 jsonHash/lockHash/cacheDir 并写入 registry（§6/§8）。
	if err := m.recordDeps(r.mf); err != nil {
		return nil, fmt.Errorf("ensure deps %s: %w", r.id, err)
	}
	// §6.4 依赖 gating：未就绪 → depsState=preparing + 后台安装（不阻塞 stateLoop/其他插件），
	// 安装完成后自动续启；当前以 ErrDepsInstalling 返回，插件置 held(IDLE) 待续启。
	if !m.pluginReady(r.mf) {
		r.mu.Lock()
		r.state = RegIdle
		r.mu.Unlock()
		m.triggerDepsInstall(r)
		return nil, ErrDepsInstalling
	}
	m.setDepsState(r.id, "ready")
	// §10.2 ② EnsureResources：依赖就绪后才对 requiresResources / externalDependencies Acquire。
	if err := m.acquireProfileResources(r.mf); err != nil {
		return nil, fmt.Errorf("ensure resources %s: %w", r.id, err)
	}
	p := New(r.mf)
	p.Gate = m.gate
	p.mgr = m
	// §5.2：内核创建插件数据目录 state/plugins/<id> 并注入；token 每次 spawn 一次性生成。
	data := ""
	if m.storeDir != "" {
		data = filepath.Join(m.storeDir, "plugins", r.mf.ID)
		if err := os.MkdirAll(data, 0o755); err != nil {
			m.releaseResourcesFor(r.mf) // 失败归还资源引用（§14.5）
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	p.SetChannel(m.newToken(), data)
	m.mu.Lock()
	sdk := m.sdkPyPath
	m.mu.Unlock()
	p.SetSdkPythonPath(sdk)
	if err := p.Start(interp); err != nil {
		m.releaseResourcesFor(r.mf) // 启动失败归还资源引用
		r.mu.Lock()
		r.state = RegStopped
		r.mu.Unlock()
		m.emitState(r, "STOPPED")
		return nil, err
	}
	// 回收后每次 spawn 重新注入崩溃回调（用当前 reg 与当前实例，避免旧实例退出误伤新实例）
	p.onExit = func(_ bool, code int) { m.onProcessExit(r, p, code) }

	r.mu.Lock()
	r.running = p
	r.state = RegRunning
	r.mu.Unlock()
	m.emitState(r, "RUNNING")

	m.applySpawnWire(p)
	m.RegisterFunctions(r.mf) // lazy 启动同样注册共享函数（幂等）
	log.Printf("[kernel] plugin %s started (load_mode=%s)", r.id, r.mf.LoadMode)
	m.ensurePatrol() // §12.2.3：启动后确保单 goroutine 巡检已就绪（首次调用时启动）
	return p, nil
}

// ensurePatrol 确保健康巡检单 goroutine 已启动（全插件共享，§12.2.3）。
// 所有启动路径（常驻/懒启动/重启）在插件就绪后调用；sync.Once 保证仅启动一次。
func (m *Manager) ensurePatrol() {
	m.patrolOnce.Do(func() { go m.patrol() })
}

// patrol 健康巡检单 goroutine（§12.2.3）：周期遍历全部运行中插件，对每个单元喂
// watchdog.Tick（心跳/忙死判定），并处理握手超时与空闲回收。取代旧的 per-plugin supervise。
func (m *Manager) patrol() {
	heartTicker := time.NewTicker(time.Second)
	defer heartTicker.Stop()
	recycleTicker := time.NewTicker(recycleCheckInterval)
	defer recycleTicker.Stop()
	for {
		select {
		case <-heartTicker.C:
			m.healthSweep()
		case <-recycleTicker.C:
			m.recallSweep()
		}
	}
}

// healthSweep 对每个运行中插件执行一次健康巡检（§12.2.3）。
func (m *Manager) healthSweep() {
	for _, r := range m.snapshotRegs() {
		r.mu.Lock()
		p := r.running
		running := r.state == RegRunning
		r.mu.Unlock()
		if p == nil || !running {
			continue
		}
		// 进程已退出 → 交由统一退出处理（退避重启/暂停）。
		if !p.Alive() {
			m.onProcessExit(r, p, p.ExitCode())
			continue
		}
		// 定时发送 ping：维持协议活性，刷新 LastResponse 作为活性证据。
		p.Ping()
		// §11.4：5s 内未握手 MUST 终止（E_HANDSHAKE_TIMEOUT）。
		if !p.Handshaken() && p.StartElapsed() > 5*time.Second {
			log.Printf("[kernel] plugin %s handshake timeout (E_HANDSHAKE_TIMEOUT); terminating", r.id)
			p.Kill()
			continue
		}
		// 组装某插件的巡检快照与探针，喂 watchdog.Tick（心跳超时 / busy 僵死）。
		st := &watchdog.Status{
			LastPong:   p.LastResponse(),
			LastActive: p.LastActivity(),
			BusyUntil:  p.BusyUntil(),
		}
		if !st.BusyUntil.IsZero() {
			st.Busy = true
		}
		toMs := r.mf.Heartbeat.TimeoutMs
		if toMs <= 0 {
			toMs = DefaultHeartbeatTimeoutMs
		}
		wd := &watchdog.Watchdog{
			HeartbeatTimeout: time.Duration(toMs) * time.Millisecond,
			OnCrashed: func(reason string) {
				log.Printf("[kernel] plugin %s %s; terminating", r.id, reason)
			},
		}
		probe := watchdog.Probe{
			Alive: func() bool { return p.Alive() },
			Ping: func() bool {
				_, err := p.Call("ping", nil, 2*time.Second)
				return err == nil
			},
		}
		if wd.Tick(st, probe) {
			p.Kill() // 触发进程退出 → healthSweep 下次探测 Alive()=false → onProcessExit
		}
	}
}

// recallSweep 空闲回收扫描（§10.3）：满足回收条件的插件优雅回收。
func (m *Manager) recallSweep() {
	for _, r := range m.snapshotRegs() {
		r.mu.Lock()
		p := r.running
		r.mu.Unlock()
		if p == nil {
			continue
		}
		if m.recycleDue(r, p) {
			m.recycle(r, p)
		}
	}
}

// recycleDue 是否满足空闲回收条件：允许回收、非常驻、无在途请求、空闲超阈值；
// §9.1 "onDemand" 关闭时机 = pending 归零立即回收（不等待空闲时长）。
func (m *Manager) recycleDue(r *reg, p *Plugin) bool {
	rc := r.mf.Recycle
	if rc.BackgroundTasks {
		return false
	}
	if r.mf.LoadMode == LoadModeAlways {
		return false // 常驻不回
	}
	if p.PendingCount() > 0 {
		return false
	}
	if rc.StopMode == "onDemand" {
		return true // 用完即关（§10.3 onDemand）
	}
	if !rc.IdleRecycle || rc.MaxIdleMs <= 0 {
		return false
	}
	return time.Since(p.LastActivity()) > time.Duration(rc.MaxIdleMs)*time.Millisecond
}

// recycle 空闲回收请求：把迁移交给状态机单 goroutine（§12.2.2），只触发不等待。
func (m *Manager) recycle(r *reg, p *Plugin) {
	m.fire(&stEvent{op: opRecycle, id: r.id, p: p})
}

// loopRecycle 状态机内回收：摘除运行态→发 shutdown→Kill→状态置 idle。
func (m *Manager) loopRecycle(r *reg, p *Plugin) {
	r.mu.Lock()
	if r.state != RegRunning {
		r.mu.Unlock()
		return
	}
	r.state = RegIdle
	r.running = nil
	r.mu.Unlock()
	m.emitState(r, "IDLE")
	log.Printf("[kernel] plugin %s idle; recycling", r.id)

	p.Stop()                    // §10.3：grace 内的 shutdown 通知 + 超时强制回收统一由 Stop() 完成
	m.releaseResourcesFor(r.mf) // §14.5：回收后释放该插件持有的资源引用
}

// onProcessExit 进程退出/崩溃上报：把迁移交给状态机单 goroutine（§12.2.2），只触发不等待。
func (m *Manager) onProcessExit(r *reg, p *Plugin, code int) {
	m.fire(&stEvent{op: opExit, id: r.id, p: p, code: code})
}

// loopExit 状态机内退出处理：按 resilience 决策是否退避重启。
// 只有当退出的实例仍是当前运行实例时才处理（避免旧实例退出误伤新实例）。
func (m *Manager) loopExit(r *reg, p *Plugin, code int) {
	r.mu.Lock()
	if r.state != RegRunning || r.running != p {
		r.mu.Unlock()
		return // 已被回收/主动停止/已被新实例替代，不重启
	}
	r.state = RegStopped
	r.running = nil
	delay, allow := m.nextBackoffLocked(r, code)
	r.mu.Unlock()
	m.emitState(r, "STOPPED")
	m.releaseResourcesFor(r.mf) // §14.10：崩溃后释放资源引用；重启路径会重新 Acquire

	if !allow {
		log.Printf("[kernel] plugin %s paused (exit code %d): auto-restart exhausted/disabled", r.id, code)
		return
	}
	log.Printf("[kernel] plugin %s crashed (exit %d); restarting in %v", r.id, code, delay)
	// 退避重启：定时器到期后投递 opStart（新 goroutine 投递，不阻塞状态机）。
	time.AfterFunc(delay, func() {
		if _, err := m.startNow(r); err != nil {
			log.Printf("[kernel] plugin %s restart failed: %v", r.id, err)
		}
	})
}

// loopStop 状态机内停止：停止运行实例→置 stopped→（可选）注销共享函数并删除登记项。
func (m *Manager) loopStop(ev *stEvent) error {
	r := m.getReg(ev.id)
	if r == nil {
		if ev.remove {
			return fmt.Errorf("plugin %s not found", ev.id)
		}
		return nil
	}
	var wasRunning bool
	r.mu.Lock()
	if p := r.running; p != nil {
		p.Stop()
		wasRunning = true
	}
	r.state = RegStopped
	r.running = nil
	r.mu.Unlock()
	if ev.remove {
		m.UnregisterFunctions(ev.id)
		m.unregisterProvidedModels(ev.id) // 转化域 §三：卸载收回该 tool 的模型声明
		m.mu.Lock()
		delete(m.regs, ev.id)
		m.mu.Unlock()
	}
	m.releaseResourcesFor(r.mf) // §14.5：停止/卸载后释放资源引用
	if wasRunning {
		m.emitState(r, "STOPPED")
	}
	return nil
}

// loopRestart 状态机内重启：停止→置 stopped→重新 spawn（同时解除依赖挂起标记）。
func (m *Manager) loopRestart(ev *stEvent) (*Plugin, error) {
	r := m.getReg(ev.id)
	if r == nil {
		mf, err := readManifest(m.pluginsDir, ev.id)
		if err != nil {
			return nil, err
		}
		r = m.register(mf)
	}
	r.mu.Lock()
	if p := r.running; p != nil {
		p.Stop()
	}
	r.state = RegStopped
	r.running = nil
	r.mu.Unlock()
	m.emitState(r, "STOPPED")
	m.releaseResourcesFor(r.mf) // §14.5：重启路径会重新 Acquire
	p, err := m.loopSpawn(r)
	if err == nil {
		// 依赖安装/修复后重启成功：解除 depsPending，恢复 GetOrStart/按需启动。
		r.mu.Lock()
		r.depsPending = false
		r.mu.Unlock()
	}
	return p, err
}

// loopStopAll 状态机内全停（内核退出时调用）。
func (m *Manager) loopStopAll() {
	for _, r := range m.snapshotRegs() {
		r.mu.Lock()
		if p := r.running; p != nil {
			p.Stop()
		}
		r.state = RegStopped
		r.running = nil
		r.mu.Unlock()
		m.releaseResourcesFor(r.mf) // §14.5：内核退出前释放全部资源引用
	}
	m.fnsMu.Lock()
	m.fns = make(map[string]Fn)
	m.fnsMu.Unlock()
}

// nextBackoffLocked 用滑动窗口计算下次重启延迟。调用方须持有 r.mu。
// 正常退出码（crash_exit_codes）或禁用自动重启 → allow=false。
func (m *Manager) nextBackoffLocked(r *reg, code int) (time.Duration, bool) {
	rc := r.mf.Resilience
	if rc.AutoRestart == nil || !*rc.AutoRestart {
		return 0, false
	}
	for _, ec := range rc.CrashExitCodes {
		if ec == code {
			return 0, false // 正常退出，不重启
		}
	}
	now := time.Now()
	win := time.Duration(rc.MaxRestartsWindowMs) * time.Millisecond
	if win <= 0 {
		win = DefaultMaxRestartsWindowMs * time.Millisecond
	}
	max := rc.MaxRestarts
	if max <= 0 {
		max = DefaultMaxRestarts
	}
	cutoff := now.Add(-win)
	fresh := r.crash[:0]
	for _, t := range r.crash {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= max {
		r.crash = fresh
		return 0, false // 窗口内超限：暂停自动重启
	}
	r.crash = append(fresh, now)

	attempt := len(r.crash) - 1
	base := rc.BackoffBaseMs
	if base <= 0 {
		base = DefaultBackoffBaseMs
	}
	backoffMax := rc.BackoffMaxMs
	if backoffMax <= 0 {
		backoffMax = DefaultBackoffMaxMs
	}
	d := base << attempt // 指数退避 base*2^attempt
	if d > backoffMax || attempt > 30 {
		d = backoffMax
	}
	return time.Duration(d) * time.Millisecond, true
}

// ── 生命周期相关管理接口 ────────────────────────────

// StopPlugin 停止并移除插件实例（释放文件句柄，供安装/卸载前调用）。
// 迁移（停止+注销+删除登记项）交由状态机单 goroutine 执行。
func (m *Manager) StopPlugin(id string) error {
	ev := &stEvent{op: opStop, id: id, remove: true}
	res := m.post(ev)
	return res.err
}

// Restart 停止并重启插件，重新解析隔离解释器（安装依赖后可用 venv）。
// 迁移（停止→spawn）交由状态机单 goroutine 执行。
func (m *Manager) Restart(id string) error {
	ev := &stEvent{op: opRestart, id: id}
	res := m.post(ev)
	return res.err
}

// Import 把 OCTplugin 格式插件目录复制进 plugins/<id> 并启动。
func (m *Manager) Import(srcPath string) error {
	mf, err := readManifestInDir(srcPath)
	if err != nil {
		return fmt.Errorf("source has no valid manifest: %w", err)
	}
	if mf.ID == "" {
		return fmt.Errorf("manifest missing id")
	}
	if err := copyDir(srcPath, filepath.Join(m.pluginsDir, mf.ID)); err != nil {
		return fmt.Errorf("copy plugin: %w", err)
	}
	mf2, err := readManifest(m.pluginsDir, mf.ID)
	if err != nil {
		return err
	}
	r := m.register(mf2)
	_, err = m.startNow(r)
	return err
}

// Remove 停止并永久删除插件（停进程 + 注销共享函数 + 删目录）。
// §5.2/§8：删除插件包与 registry 条目，但 MUST NOT 删除 state/plugins/<id> 数据目录。
func (m *Manager) Remove(id string) error {
	if err := m.StopPlugin(id); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(filepath.Join(m.pluginsDir, id)); err != nil {
		return err
	}
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rs != nil {
		if err := rs.RemovePlugin(id); err != nil {
			log.Printf("[kernel] registry: remove %s: %v", id, err)
		}
	}
	return nil
}

// List 返回当前运行中的插件 id。
func (m *Manager) List() []string {
	out := []string{}
	for _, r := range m.snapshotRegs() {
		r.mu.Lock()
		if r.state == RegRunning {
			out = append(out, r.id)
		}
		r.mu.Unlock()
	}
	sort.Strings(out)
	return out
}

// PluginSummary 一个插件注册摘要（含未启动的 lazy/idle/disabled）。
type PluginSummary struct {
	Manifest
	State    string `json:"state"`
	Disabled bool   `json:"disabled,omitempty"`
}

// All 返回全部已注册插件的摘要（含未启动），供宿主渲染侧栏/管理列表 —— 不要求 running。
// lazy/prewarm/disabled 插件即使未启动也能被列出与详情查看。
func (m *Manager) All() []PluginSummary {
	regs := m.snapshotRegs()
	out := make([]PluginSummary, 0, len(regs))
	for _, r := range regs {
		r.mu.Lock()
		out = append(out, PluginSummary{
			Manifest: r.mf,
			State:    r.state.String(),
			Disabled: r.disabled,
		})
		r.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Describe 返回某插件的注册信息（即使未运行），ok=false 表示未知插件。
func (m *Manager) Describe(id string) (Manifest, bool) {
	r := m.getReg(id)
	if r == nil {
		return Manifest{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mf, true
}

// ── 阶段C · 共享函数注册表（FR-8） ────────────────

func (m *Manager) RegisterFunctions(mf Manifest) {
	m.fnsMu.Lock()
	defer m.fnsMu.Unlock()
	for _, f := range mf.Functions {
		m.fns[f.Name] = Fn{Name: f.Name, Method: f.Method, Desc: f.Desc, PluginID: mf.ID}
	}
}

// CommandItem 命令面板中的一条可执行命令（FR-9）。
type CommandItem struct {
	Name     string         `json:"name"`
	Desc     string         `json:"desc"`
	PluginID string         `json:"pluginId,omitempty"`
	Method   string         `json:"method,omitempty"`
	Params   []CommandParam `json:"params,omitempty"`
}

// Commands 聚合所有插件 manifest.commands 中声明了 method 的可执行命令。
func (m *Manager) Commands() []CommandItem {
	var out []CommandItem
	for _, r := range m.snapshotRegs() {
		for _, c := range r.mf.Commands {
			if c.Method == "" {
				continue
			}
			out = append(out, CommandItem{
				Name: c.Name, Desc: c.Desc, PluginID: r.id, Method: c.Method, Params: c.Params,
			})
		}
	}
	return out
}

func (m *Manager) UnregisterFunctions(pluginID string) {
	m.fnsMu.Lock()
	defer m.fnsMu.Unlock()
	for k, v := range m.fns {
		if v.PluginID == pluginID {
			delete(m.fns, k)
		}
	}
}

func (m *Manager) FuncList() []Fn {
	m.fnsMu.Lock()
	defer m.fnsMu.Unlock()
	out := make([]Fn, 0, len(m.fns))
	for _, v := range m.fns {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FuncOwner 返回共享函数所属插件 ID（不存在返回空串）。用于把调用失败映射到
// 具体插件（如依赖安装中 → 携带 pluginId 的错误）。
func (m *Manager) FuncOwner(name string) string {
	m.fnsMu.Lock()
	defer m.fnsMu.Unlock()
	return m.fns[name].PluginID
}

// CallFunc 按共享函数名路由到所属插件执行（懒启动：未运行先 GetOrStart）。
func (m *Manager) CallFunc(name string, params any, timeout time.Duration) (protocol.Response, error) {
	m.fnsMu.Lock()
	f, ok := m.fns[name]
	m.fnsMu.Unlock()
	if !ok {
		return protocol.Response{}, fmt.Errorf("function %q not registered", name)
	}
	pl, err := m.GetOrStart(f.PluginID)
	if err != nil {
		return protocol.Response{}, err
	}
	if !pl.Alive() {
		return protocol.Response{}, fmt.Errorf("plugin %s down", f.PluginID)
	}
	return pl.Call(f.Method, params, timeout)
}

// StopAll 停止全部运行中的插件（内核退出时调用）。
func (m *Manager) StopAll() {
	m.post(&stEvent{op: opStopAll})
}
