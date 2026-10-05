package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/octplugin/kernel/internal/perms"
	"github.com/octplugin/kernel/internal/process"
	"github.com/octplugin/kernel/internal/transport"
	"github.com/octplugin/kernel/pkg/protocol"
)

// Manifest 描述文件声明（schema）。生命周期策略见 LifecyclePolicy（supervisor/lifecycle.go）。
type Manifest struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"` // §7.5/§8：registry 记录 manifestHash 的版本基准；缺失时为 ""
	Type    string `json:"type"`
	Entry   string `json:"entry"` // 相对插件目录的入口文件，如 main.py
	UIMode  string `json:"ui_mode"`
	Dir     string `json:"-"`
	Py3v    string `json:"python_version,omitempty"` // 默认 "3.12"
	// Kind 插件/工具类别（§：app 普通插件 | tool 无 UI 工具）。tool 复用同一进程管理通道，
	// 仅在侧栏/设置作外显区分。缺省为 "app"。
	Kind string `json:"kind,omitempty"`
	LifecyclePolicy
	ManifestFields
}

// Plugin 一个已启动/待启动插件进程的运行实例（低层：进程、stdin/stdout 协议、活性计量）。
// 生命周期状态机（start/idle/stop/backoff-restart）由 Manager 驱动，Plugin 只负责：
//   - 派生子进程、读写 JSON-RPC 行；
//   - 维护 lastPong（任何成功读行视为活性）、lastActivity（空闲回收依据）；
//   - 进程退出回收（Wait/exitCode），并回调 onExit 通知 Manager。
type Plugin struct {
	ID       string
	Manifest Manifest
	Gate     *perms.Gate                     // 敏感操作拦截（插件→内核 gate 请求）
	mgr      *Manager                        // 跨插件 registry.call 路由
	Event    func(src, typ string, data any) // 插件→内核→宿主事件广播出口

	proc        *exec.Cmd
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	limitCloser func()                     // 释放 Job Object 等资源
	onExit      func(crash bool, code int) // 由 Manager 注入；进程退出后回调
	onExitOnce  sync.Once
	waitOnce    sync.Once

	// §5.2 / §11.4：内核注入的通道与数据目录（由 Manager.spawn 经 SetChannel 下发）。
	channelToken  string
	dataDir       string
	sdkPythonPath string // §16.5：oct_sdk 根，经 PYTHONPATH 注入

	mu           sync.Mutex
	nextID       int
	pending      map[int]chan protocol.Response
	lastPong     time.Time
	lastActivity time.Time
	busyUntil    time.Time // §10.1.2 修饰字段：非零 = busy 中，值即 busyDeadline
	alive        bool
	exitCode     int
	exitCh       chan struct{} // handleExit 后关闭，供 Stop/supervise 等待

	// §11.4：握手校验状态。spawn 后须在 5s 内收到合法 $/handshake 帧，
	// 否则由 supervise 判 E_HANDSHAKE_TIMEOUT 终止；读循环对首帧做 token/version 校验。
	handshaken bool
	startedAt  time.Time // spawn 成功时刻，供握手超时判定
}

// LocatePython 用 uv 定位托管解释器（only-managed，不依赖系统 Python）。
// 带超时保护：uv 缺失/卡住时快速失败，不阻塞内核启动。
func LocatePython(version string) (string, error) {
	uvBin := "uv"
	if v := os.Getenv("OCTRUN_UV"); v != "" {
		uvBin = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, uvBin, "python", "find", version).Output()
	if err != nil {
		return "", fmt.Errorf("uv python find %s failed: %w (ensure uv in PATH or OCTRUN_UV)", version, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func New(manifest Manifest) *Plugin {
	return &Plugin{
		ID:       manifest.ID,
		Manifest: manifest,
		pending:  make(map[int]chan protocol.Response),
		exitCh:   make(chan struct{}),
	}
}

// SetChannel 注入 §5.2/§11.4 的信道 token 与插件数据目录（spawn 前调用）。
// 数据目录由内核创建并保证可写；OCT_PLUGIN_HOME 直接用 Manifest.Dir（只读）。
func (p *Plugin) SetChannel(token, dataDir string) {
	p.channelToken = token
	p.dataDir = dataDir
}

// SetSdkPythonPath 注入 oct_sdk 根（§16.5），spawn 时经 PYTHONPATH 使插件可 import。
func (p *Plugin) SetSdkPythonPath(sdkPythonPath string) {
	p.sdkPythonPath = sdkPythonPath
}

// Start 启动插件子进程并开始读循环。解释器由调用方（Manager）解析。
// Go 插件：pythonPath 即二进制绝对路径，直接执行（不再拼接 entry）。
func (p *Plugin) Start(pythonPath string) error {
	var cmd *exec.Cmd
	if isGoType(p.Manifest.Type) {
		// Go 编译型插件：pythonPath 已是二进制绝对路径，直接执行。
		cmd = exec.Command(pythonPath)
	} else {
		pyVersion := p.Manifest.Py3v
		if pyVersion == "" {
			pyVersion = "3.12"
		}
		cmd = exec.Command(pythonPath, filepath.Join(p.Manifest.Dir, p.Manifest.Entry))
	}
	cmd.Dir = p.Manifest.Dir
	// §13.2：Starts 前建立进程隔离（Unix 进程组 / Windows 由 hideConsoleWindow 的 CREATE_SUSPENDED 接管）。
	process.PreStartAttrs(cmd)
	// §5.2/§11.2/§11.4：注入数据目录、握手 token、环境清理（stdout 协议专用）。
	// Go 插件不注入 PYTHONUNBUFFERED/PYTHONPATH（SDK 路径对其无意义）。
	if isGoType(p.Manifest.Type) {
		cmd.Env = process.BuildPluginEnv(os.Environ(), p.channelToken, p.Manifest.Dir, p.dataDir, "")
	} else {
		cmd.Env = process.BuildPluginEnv(os.Environ(), p.channelToken, p.Manifest.Dir, p.dataDir, p.sdkPythonPath)
	}
	hideConsoleWindow(cmd) // Windows: 不弹黑窗口；其它平台：空实现

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	// §13.1/§12.3：无条件建立 Job（tree-reap + 可选内存上限），Assign 后恢复进程；
	// 失败 MUST 中止启动，MUST NOT 降级为裸跑。
	closer, err := attachLifecycle(cmd, p.Manifest.Limits.MemBytes)
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	p.limitCloser = closer

	// §11.4：记录 spawn 成功时刻，供 supervise 判定握手超时（5s）。
	p.mu.Lock()
	p.startedAt = time.Now()
	p.mu.Unlock()

	p.prepare(cmd, stdout, stdin)
	go p.readLoop()
	return nil
}

func (p *Plugin) prepare(cmd *exec.Cmd, stdoutPipe io.ReadCloser, stdinPipe io.WriteCloser) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.proc = cmd
	p.stdin = stdinPipe
	p.stdout = stdoutPipe
	p.alive = true
	p.lastPong = time.Now()
	p.lastActivity = time.Now()
	p.exitCode = -1
}

// readLoop 逐行解析插件 stdout（JSON-RPC）。任何成功读行都刷新 lastPong（活性）与 lastActivity。
// §11.4：首帧 MUST 为非污染 JSON 且为合法 $/handshake（token 与 protocolVersion 校验通过），
// 否则终止该单元并记 E_HANDSHAKE_REJECTED。
// §11.3：以 transport.Deframer 即时校验每一行——非法 JSON（stdout 污染）立即终止该单元，
// 不静默丢弃；超长行（> limits.stdout_line_bytes）由 Deframer 切断并告警，不终止读循环。
func (p *Plugin) readLoop() {
	maxLine := p.Manifest.Limits.StdoutLineBytes
	if maxLine <= 0 {
		maxLine = DefaultStdoutLineBytes
	}
	first := true
	d := transport.NewDeframer(p.ID, maxLine, func(msg json.RawMessage) {
		if first {
			first = false
			// §11.4：移交握手；握手被拒时进程已 Kill，随后的 EOF 触发 handleExit。
			if !p.attemptHandshake(msg) {
				return
			}
			return
		}
		p.touch()
		p.handleLine(msg)
	})
	d.SetLongLineCallback(func(unit string, n int) { logSkipped(unit, n) })
	buf := make([]byte, 32*1024)
	for {
		n, err := p.stdout.Read(buf)
		if n > 0 {
			// §11.3：非法行 → 协议污染 → 立即终止该单元（E_STDOUT_CONTAMINATED）。
			if ferr := d.Feed(buf[:n]); ferr != nil {
				p.rejectHandshake(ferr.Error())
				break
			}
		}
		if err != nil {
			break
		}
	}
	p.handleExit()
}

// attemptHandshake 校验首帧是否合法 $/handshake（§11.4 MUST）。已认证幂等返回 true。
func (p *Plugin) attemptHandshake(line []byte) bool {
	p.mu.Lock()
	if p.handshaken {
		p.mu.Unlock()
		return true
	}
	p.mu.Unlock()

	f, err := transport.ParseHandshake(line)
	if err != nil {
		p.rejectHandshake("bad handshake frame: " + err.Error())
		return false
	}
	res := transport.VerifyHandshake(f, p.channelToken, protocol.ProtocolVersion)
	if !res.OK {
		// §11.4：token / protocolVersion 不匹配 → E_HANDSHAKE_REJECTED，记录安全事件。
		p.rejectHandshake(res.Msg)
		return false
	}
	p.mu.Lock()
	p.handshaken = true
	p.lastPong = time.Now() // 握手帧也是活性证据（§13.3）
	p.lastActivity = time.Now()
	p.mu.Unlock()
	log.Printf("[kernel] plugin %s handshake ok", p.ID)
	return true
}

// rejectHandshake 握手校验失败：记录安全事件并终止该进程（触发 onExit → 退避/禁用决策）。
func (p *Plugin) rejectHandshake(reason string) {
	log.Printf("[kernel] plugin %s handshake rejected (E_HANDSHAKE_REJECTED): %s; terminating", p.ID, reason)
	p.Kill()
}

// Handshaken 是否已通过 §11.4 握手认证。
func (p *Plugin) Handshaken() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handshaken
}

// StartElapsed 距 spawn 成功的耗时（零值 startedAt 返回 0）。
func (p *Plugin) StartElapsed() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.startedAt.IsZero() {
		return 0
	}
	return time.Since(p.startedAt)
}

func (p *Plugin) handleLine(line []byte) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return
	}
	if _, isReq := raw["method"]; isReq {
		var req protocol.Request
		if json.Unmarshal(line, &req) != nil {
			return
		}
		p.dispatchGate(req.ID, req.Method, req.Params)
		return
	}
	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return
	}
	if resp.ID == 0 {
		return
	}
	p.mu.Lock()
	ch := p.pending[int(resp.ID)]
	delete(p.pending, int(resp.ID))
	p.mu.Unlock()
	if ch != nil {
		ch <- resp
	}
}

// logSkipped 在 Deframer 丢弃超长 stdout 行时打告警（§11.3，不影响读循环）。
func logSkipped(id string, n int) {
	log.Printf("[plugin %s] stdout line exceeded %d bytes; truncated and skipped", id, n)
}

// handleExit 进程退出：回收（Wait→exit code），唤醒 pending，回调 onExit。
func (p *Plugin) handleExit() {
	p.waitOnce.Do(func() {
		if p.proc != nil {
			_ = p.proc.Wait()
			if p.proc.ProcessState != nil {
				p.exitCode = p.proc.ProcessState.ExitCode()
			}
		}
	})
	p.mu.Lock()
	p.alive = false
	for id, ch := range p.pending {
		ch <- protocol.NewError(int64(id), protocol.ErrPluginCrashed, nil)
		delete(p.pending, id)
	}
	p.mu.Unlock()
	close(p.exitCh)

	p.onExitOnce.Do(func() {
		if cb := p.onExit; cb != nil {
			cb(true, p.exitCode) // crash=true：受监管期间退出（回收/举报给 Manager 决策）
		}
	})
	if p.limitCloser != nil {
		p.limitCloser()
	}
}

// SetBusy 登记/释放 busy（§10.5）：maxBusyMs>0 登记 busyUntil，0 释放。
// busy 期间豁免空闲杀死与关闭策略；超时由 supervise 巡检强制探活。
func (p *Plugin) SetBusy(maxBusyMs int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if maxBusyMs > 0 {
		p.busyUntil = time.Now().Add(time.Duration(maxBusyMs) * time.Millisecond)
		return
	}
	p.busyUntil = time.Time{}
}

// BusyUntil 返回 busy 截止时间（零值 = 非 busy）。
func (p *Plugin) BusyUntil() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.busyUntil
}

// dispatchGate 处理插件对受保护服务的请求：先过权限位图，未授权统一 -32005。
func (p *Plugin) dispatchGate(id int64, method string, params []byte) {
	var code int
	var ok bool
	var result any
	var data any

	switch method {
	case "gate.file_read":
		var pr struct {
			Perm string `json:"perm"`
			Path string `json:"path"`
		}
		_ = json.Unmarshal(params, &pr)
		if pr.Perm == "" {
			pr.Perm = perms.FileRead
		}
		if p.Gate != nil && p.Gate.Check(p.ID, pr.Perm) != nil {
			code, ok, data = protocol.ErrPermDenied, false, map[string]any{"pluginId": p.ID, "perm": pr.Perm}
			break
		}
		b, err := os.ReadFile(pr.Path)
		if err != nil {
			code, ok, data = protocol.ErrIO, false, map[string]any{"error": err.Error()}
			break
		}
		ok, result = true, map[string]any{"path": pr.Path, "bytes": len(b)}
	case "gate.file_write":
		var pw struct {
			Path string `json:"path"`
			Data string `json:"data"` // UTF-8 字符串按原样写入（append:false 覆盖）
		}
		_ = json.Unmarshal(params, &pw)
		if p.Gate != nil && p.Gate.Check(p.ID, perms.FileWrite) != nil {
			code, ok, data = protocol.ErrPermDenied, false, map[string]any{"pluginId": p.ID, "perm": perms.FileWrite}
			break
		}
		if err := os.WriteFile(pw.Path, []byte(pw.Data), 0o644); err != nil {
			code, ok, data = protocol.ErrIO, false, map[string]any{"error": err.Error()}
			break
		}
		ok, result = true, map[string]any{"path": pw.Path, "bytes": len(pw.Data)}
	case "gate.execute_command":
		var pc struct {
			Command   string   `json:"command"` // 可执行文件路径（不经过 shell，防注入）
			Args      []string `json:"args"`
			TimeoutMs int64    `json:"timeoutMs"`
		}
		_ = json.Unmarshal(params, &pc)
		if p.Gate != nil && p.Gate.Check(p.ID, perms.ExecuteCommand) != nil {
			code, ok, data = protocol.ErrPermDenied, false, map[string]any{"pluginId": p.ID, "perm": perms.ExecuteCommand}
			break
		}
		cmd := exec.Command(pc.Command, pc.Args...)
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		if err := cmd.Start(); err != nil {
			code, ok, data = protocol.ErrIO, false, map[string]any{"error": err.Error()}
			break
		}
		ms := pc.TimeoutMs
		if ms <= 0 {
			ms = 15000
		}
		t := time.AfterFunc(time.Duration(ms)*time.Millisecond, func() { cmd.Process.Kill() })
		e := cmd.Wait()
		t.Stop()
		if e != nil {
			code, ok, data = protocol.ErrIO, false, map[string]any{"error": e.Error(), "stdout": out.String(), "stderr": errb.String()}
			break
		}
		ok, result = true, map[string]any{"stdout": out.String(), "stderr": errb.String()}
	case "gate.file_exists":
		var pr struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(params, &pr)
		_, err := os.Stat(pr.Path)
		ok, result = true, map[string]any{"exists": err == nil}
	case "registry.call":
		var pr struct {
			Name      string          `json:"name"`
			Params    json.RawMessage `json:"params"`
			TimeoutMs *int            `json:"timeoutMs"`
		}
		_ = json.Unmarshal(params, &pr)
		if p.mgr == nil {
			code, ok, data = protocol.ErrMethodNotFound, false, nil
			break
		}
		var in any
		if len(pr.Params) > 0 {
			_ = json.Unmarshal(pr.Params, &in)
		}
		// 默认 15s；调用方（如 conversion 编排）可经 timeoutMs 放宽，
		// 与宿主侧 ws.go handleRegistryCall 的语义一致（§11.6）。
		timeout := 15 * time.Second
		if pr.TimeoutMs != nil && *pr.TimeoutMs > 0 {
			timeout = time.Duration(*pr.TimeoutMs) * time.Millisecond
		}
		resp, err := p.mgr.CallFunc(pr.Name, in, timeout)
		if err != nil {
			code, ok, data = protocol.ErrPluginMissing, false, map[string]any{"error": err.Error()}
			break
		}
		if resp.Error != nil {
			code, ok, data = resp.Error.Code, false, resp.Error.Data
			break
		}
		ok, result = true, map[string]any{"ok": true, "result": resp.Result}
	// gate.model_ensure 转化域 §22.3 / §四：插件只能「请求宿主确保模型就绪」，
	// 由宿主按 §15.4 解析并（必要时）按 §12 走 handover。需 local_model 权限位。
	case "gate.model_ensure":
		var pm struct {
			Capability string `json:"capability"`
			ModelID    string `json:"modelId"`
			ID         string `json:"id"`
			TimeoutMs  int    `json:"timeoutMs"`
		}
		_ = json.Unmarshal(params, &pm)
		if pm.ModelID == "" {
			pm.ModelID = pm.ID
		}
		if p.Gate != nil && p.Gate.Check(p.ID, perms.LocalModel) != nil {
			code, ok, data = protocol.ErrPermDenied, false, map[string]any{"pluginId": p.ID, "perm": perms.LocalModel}
			break
		}
		if p.mgr == nil {
			code, ok = protocol.ErrMethodNotFound, false
			break
		}
		// §23.1：插件只能请求自己在 requiresCapabilities 里声明过的能力。
		// （manifest 未声明该字段时不做约束，保持向后兼容。）
		if pm.Capability != "" {
			if _, allowed := requiresCapability(p.Manifest, pm.Capability); !allowed {
				code = protocol.ErrPermDenied
				ok = false
				data = map[string]any{
					"diag":       protocol.DiagPermissionDenied,
					"pluginId":   p.ID,
					"capability": pm.Capability,
					"declared":   p.Manifest.RequiresCapabilities,
					"error": fmt.Sprintf("插件 %s 未在 requiresCapabilities 中声明能力 %q（§23.1）",
						p.ID, pm.Capability),
				}
				break
			}
		}
		to := ensureTimeout
		if pm.TimeoutMs > 0 {
			to = time.Duration(pm.TimeoutMs) * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(context.Background(), to)
		defer cancel()
		er, err := p.mgr.EnsureModel(ctx, pm.Capability, pm.ModelID)
		if err != nil {
			code, ok, data = protocol.ErrPluginMissing, false, map[string]any{"error": err.Error()}
			break
		}
		ok, result = true, map[string]any{"ok": true, "model": er}
	// gate.models_list 转化域 §22.4/§25：conversion 代理渲染模型选择器所需的
	// registry.json.models 视图（含 pinned/effective/unavailable）。需 local_model 权限位。
	case "gate.models_list":
		if p.Gate != nil && p.Gate.Check(p.ID, perms.LocalModel) != nil {
			code, ok, data = protocol.ErrPermDenied, false, map[string]any{"pluginId": p.ID, "perm": perms.LocalModel}
			break
		}
		if p.mgr == nil {
			code, ok = protocol.ErrMethodNotFound, false
			break
		}
		pins := p.mgr.pins()
		ok, result = true, map[string]any{
			"ok":           true,
			"models":       p.mgr.ModelsSnapshot(pins),
			"capabilities": p.mgr.CapabilitiesSnapshot(pins),
			"pins":         pins,
			"contracts":    p.mgr.Contracts(),
		}
	// gate.models_pin 转化域 §8.1：写入/清除能力级 pin（设置级持久默认）。需 local_model 权限位。
	case "gate.models_pin":
		var pp struct {
			Capability string `json:"capability"`
			ModelID    string `json:"modelId"`
		}
		_ = json.Unmarshal(params, &pp)
		if p.Gate != nil && p.Gate.Check(p.ID, perms.LocalModel) != nil {
			code, ok, data = protocol.ErrPermDenied, false, map[string]any{"pluginId": p.ID, "perm": perms.LocalModel}
			break
		}
		if p.mgr == nil {
			code, ok = protocol.ErrMethodNotFound, false
			break
		}
		if pp.Capability == "" && pp.ModelID != "" {
			pp.Capability = p.mgr.CapabilityOf(pp.ModelID)
		}
		if pp.Capability == "" {
			code, ok, data = protocol.ErrParse, false, map[string]any{"error": "capability required"}
			break
		}
		if err := p.mgr.SetPin(pp.Capability, pp.ModelID); err != nil {
			code, ok, data = protocol.ErrIO, false, map[string]any{"error": err.Error()}
			break
		}
		ok, result = true, map[string]any{
			"ok": true, "capability": pp.Capability, "pinned": pp.ModelID,
			"resolution": p.mgr.ResolveCapability(pp.Capability, p.mgr.pins()),
		}
	case "event.emit":
		var pr struct {
			Type string `json:"type"`
			Data any    `json:"data"`
		}
		_ = json.Unmarshal(params, &pr)
		if p.Event != nil {
			p.Event(p.ID, pr.Type, pr.Data)
		}
		ok, result = true, map[string]any{"ok": true}
	// §17.2 A：插件读取「自己的」生效设置（user-settings plugins[id].settings 经 manifest 默认值
	// 合并）。设置由宿主「设置→插件/tool内部设置」页按 settingsSchema 渲染编辑；插件不直读
	// config/user-settings.json（内核独占写，§17.3）。需 local_model 权限位。
	case "gate.settings_get":
		if p.Gate != nil && p.Gate.Check(p.ID, perms.LocalModel) != nil {
			code, ok, data = protocol.ErrPermDenied, false, map[string]any{"pluginId": p.ID, "perm": perms.LocalModel}
			break
		}
		if p.mgr == nil {
			code, ok = protocol.ErrMethodNotFound, false
			break
		}
		eff, err := p.mgr.SettingsEffective(p.ID, p.Manifest)
		if err != nil {
			code, ok, data = protocol.ErrIO, false, map[string]any{"error": err.Error()}
			break
		}
		if eff == nil {
			eff = map[string]any{}
		}
		ok, result = true, map[string]any{"ok": true, "settings": eff}
	// §17.2 A：插件写「自己的」设置（仅写 settingsSchema 声明字段）。
	// capability-gateway.setPreference 的落盘入口。
	case "gate.settings_set":
		if p.Gate != nil && p.Gate.Check(p.ID, perms.LocalModel) != nil {
			code, ok, data = protocol.ErrPermDenied, false, map[string]any{"pluginId": p.ID, "perm": perms.LocalModel}
			break
		}
		if p.mgr == nil {
			code, ok = protocol.ErrMethodNotFound, false
			break
		}
		var ps struct {
			Settings map[string]any `json:"settings"`
		}
		_ = json.Unmarshal(params, &ps)
		if err := p.mgr.SetSettings(p.ID, p.Manifest, ps.Settings); err != nil {
			code, ok, data = protocol.ErrIO, false, map[string]any{"error": err.Error()}
			break
		}
		ok, result = true, map[string]any{"ok": true}
	// §5.5 隐私权限断言：插件在执行敏感动作（屏幕采集/麦克风录音）前主动断言权限位。
	// 未授权返回 E_PERM_DENIED，由插件转为对用户的明确报错（不打哑炮）。
	case "gate.perm_assert":
		var pa struct {
			Perm string `json:"perm"`
		}
		_ = json.Unmarshal(params, &pa)
		if p.Gate == nil {
			code, ok = protocol.ErrPluginState, false
			break
		}
		// 只允许断言已声明（manifest permissions）的权限位，防止试探未声明的高敏权限。
		if err := p.Gate.CheckDeclared(p.ID, pa.Perm); err != nil {
			code, ok, data = protocol.ErrParse, false, map[string]any{"pluginId": p.ID, "perm": pa.Perm, "error": err.Error()}
			break
		}
		if err := p.Gate.Check(p.ID, pa.Perm); err != nil {
			code, ok, data = protocol.ErrPermDenied, false, map[string]any{"pluginId": p.ID, "perm": pa.Perm}
			break
		}
		ok, result = true, map[string]any{"ok": true, "perm": pa.Perm}
	case "sdk.busy": // §10.5：申请/释放 busy（maxBusyMs>0 申请，0 释放）
		var pr struct {
			MaxBusyMs int64 `json:"maxBusyMs"`
		}
		_ = json.Unmarshal(params, &pr)
		p.SetBusy(pr.MaxBusyMs)
		ok, result = true, map[string]any{"ok": true}
	default:
		code, ok, data = protocol.ErrMethodNotFound, false, nil
	}
	p.writeGateReply(id, code, ok, result, data)
}

func (p *Plugin) writeGateReply(id int64, code int, ok bool, result, data any) {
	var r protocol.Response
	if ok {
		r = protocol.NewResult(id, result)
	} else {
		r = protocol.NewError(id, code, data)
	}
	b, _ := json.Marshal(r)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.alive {
		_, _ = p.stdin.Write(append(b, '\n'))
	}
}

func (p *Plugin) touch() {
	p.mu.Lock()
	p.lastPong = time.Now()
	p.lastActivity = time.Now()
	p.mu.Unlock()
}

// Call 向插件发一次性请求，阻塞等响应（带超时）。发出即视为一次活动。
func (p *Plugin) Call(method string, params any, timeout time.Duration) (protocol.Response, error) {
	p.mu.Lock()
	if !p.alive {
		p.mu.Unlock()
		return protocol.Response{}, fmt.Errorf("plugin %s not alive", p.ID)
	}
	p.nextID++
	id := p.nextID
	ch := make(chan protocol.Response, 1)
	p.pending[id] = ch
	p.lastActivity = time.Now()
	req := protocol.Request{V: protocol.ProtocolVersion, JSONRPC: "2.0", ID: int64(id), Method: method}
	if params != nil {
		b, _ := json.Marshal(params)
		req.Params = b
	}
	b, _ := json.Marshal(req)
	_, err := p.stdin.Write(append(b, '\n'))
	p.mu.Unlock()
	if err != nil {
		return protocol.Response{}, err
	}

	var resp protocol.Response
	var timeoutErr error
	select {
	case resp = <-ch:
		return resp, nil
	case <-time.After(timeout):
		timeoutErr = fmt.Errorf("plugin %s call %s timeout", p.ID, method)
	}
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
	return protocol.Response{}, timeoutErr
}

// Ping 发送心跳（不更新 lastPong——活性以 pong/任何读行为准）。
func (p *Plugin) Ping() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.alive {
		return
	}
	p.nextID++
	id := p.nextID
	req := protocol.Request{V: protocol.ProtocolVersion, JSONRPC: "2.0", ID: int64(id), Method: "ping"}
	b, _ := json.Marshal(req)
	_, _ = p.stdin.Write(append(b, '\n'))
}

func (p *Plugin) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.alive
}

// LastResponse 最近一次成功读行（活性）时间。
func (p *Plugin) LastResponse() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastPong
}

// LastActivity 最近一次活动时间（空闲回收依据）。
func (p *Plugin) LastActivity() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastActivity
}

// ExitCh 进程退出通知 channel（handleExit 后关闭）。
func (p *Plugin) ExitCh() <-chan struct{} { return p.exitCh }

// ExitCode 进程退出码（未退出时为 -1）。
func (p *Plugin) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}

// PendingCount 进行中的业务请求数（空闲回收据此判断是否有在途请求）。
func (p *Plugin) PendingCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

// Kill 强制终止进程（含进程树，§13）并等待回收（读循环会在 EOF 后自行 handleExit/onExit）。
func (p *Plugin) Kill() {
	p.mu.Lock()
	alive := p.alive
	p.mu.Unlock()
	if p.proc != nil && alive {
		process.KillTree(p.proc)
	}
}

// Stop 停止插件（§10.3 MUST NOT 只做 Kill 而跳过优雅阶段）：
// 对已握手成功的业务进程先发 shutdown 通知、等待 graceMs 让其 flush 数据/做清理，
// 超时或未成功握手再强制 KillTree 整树回收（§13）。最后等待读循环回收，
// 确保进程真正退出、句柄释放（供 Install 时删 venv）。
func (p *Plugin) Stop() {
	p.mu.Lock()
	alive := p.alive
	handshaken := p.handshaken
	p.mu.Unlock()
	grace := p.Manifest.Recycle.GracefulShutdownMs
	if grace <= 0 {
		grace = DefaultGracefulShutdownMs
	}
	if p.proc != nil && alive {
		if handshaken {
			// §10.3：先优雅——发 shutdown 并在 grace 窗口内等插件自行清理/退出。
			done := make(chan struct{})
			go func() {
				_, _ = p.Call("shutdown", nil, time.Duration(grace)*time.Millisecond)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Duration(grace) * time.Millisecond):
			}
		}
		// 若插件未在 grace 内自行退出，强制终止整棵进程树。
		p.mu.Lock()
		still := p.alive
		p.mu.Unlock()
		if still {
			process.KillTree(p.proc)
		}
	}
	select {
	case <-p.exitCh:
	case <-time.After(time.Duration(grace)*time.Millisecond + time.Second):
	}
}
