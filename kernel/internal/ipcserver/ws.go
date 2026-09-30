package ipcserver

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/octplugin/kernel/internal/config"
	"github.com/octplugin/kernel/internal/lifecycle"
	"github.com/octplugin/kernel/internal/perms"
	"github.com/octplugin/kernel/internal/runtime"
	"github.com/octplugin/kernel/pkg/protocol"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // 仅监听 127.0.0.1，见 Listen
}

// Server 内核对外 WS + 资源服务入口。内核与宿主 1:1 长连接。
type Server struct {
	authToken    string
	smanager     *lifecycle.Manager
	gate         *perms.Gate
	installer    *runtime.Installer
	resourcesDir string           // 阶段E：共享资源根目录（内核对宿主/插件暴露 resources/）
	pluginsDir   string           // 阶段G：插件 UI 静态文件根（内核对宿主 iframe 暴露 /plugin/）
	stateDir     string           // §17.3：内核独占写 state/；host 经 RPC 读写，不得直连文件
	settings     *config.Settings // §9：config/user-settings.json（settingsSchema 驱动设置页的落点）
	mu           sync.Mutex
	conn         *websocket.Conn
	writeMu      sync.Mutex // 串行化所有 WS 写，避免并发 WriteJSON panic
}

// HostClientName 标识宿主主连接；其余 client 视为插件 iframe 连接。
const HostClientName = "octplugin-host"

// SetSettingsStore 注入 user-settings.json 句柄（§9/§17.2：宿主设置页的读写通道）。
func (s *Server) SetSettingsStore(st *config.Settings) { s.settings = st }

// SetStateDir 注入 state/ 根（§17.3：内核独占写；host 一律经 RPC 读写，不得直连文件）。
func (s *Server) SetStateDir(dir string) { s.stateDir = dir }

// setHost 把宿主连接设为事件广播目标；重复连接时替换并关闭旧宿主连接。
func (s *Server) setHost(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && s.conn != conn {
		_ = s.conn.Close()
	}
	s.conn = conn
}

func NewServer(token string, mgr *lifecycle.Manager, gate *perms.Gate, inst *runtime.Installer, resourcesDir, pluginsDir string) *Server {
	return &Server{authToken: token, smanager: mgr, gate: gate, installer: inst, resourcesDir: resourcesDir, pluginsDir: pluginsDir}
}

// WireEvents 把单长连接写者注入当前运行插件，作为插件→内核 event 广播出口。
// 需在 manager.StartAll() 之后调用（否则插件尚未启动）。
func (s *Server) WireEvents() {
	for _, id := range s.smanager.List() {
		if pl := s.smanager.Plugin(id); pl != nil {
			pl.Event = func(src, typ string, data any) { s.broadcast(src, typ, data) }
		}
	}
}

// EventSink 返回一个插件→内核→宿主的广播闭包，供懒启动/重启的新插件实例注入 Event。
func (s *Server) EventSink() func(src, typ string, data any) {
	return func(src, typ string, data any) { s.broadcast(src, typ, data) }
}

// NotifyState 由内核 Manager 的状态变更回调注入，把插件进程启停推给宿主（source=kernel, type=plugin.state）。
func (s *Server) NotifyState(id, state string) {
	s.broadcast("kernel", "plugin.state", map[string]any{"id": id, "state": state})
}

// NotifyDepsProgress 由内核 Manager 的安装进度回调注入，把依赖安装阶段推给宿主（§6.4 MUST）。
func (s *Server) NotifyDepsProgress(id, phase string) {
	s.broadcast("kernel", "plugin.deps.progress", map[string]any{"id": id, "phase": phase})
}

// broadcast 把插件事件以 ws 通知（无 id）推给宿主（FR-8 事件通道）。
func (s *Server) broadcast(src, typ string, data any) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.WriteJSON(protocol.Notification{ // 无 id 的通知（事件通道）
		V: protocol.ProtocolVersion, JSONRPC: "2.0", Method: "event",
		Params: map[string]any{"source": src, "type": typ, "data": data},
	})
}

// Listen 绑定 localhost 随机端口，返回监听器与端口。
func (s *Server) Listen(addr string) (net.Listener, int, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, 0, err
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

func (s *Server) Serve(ln net.Listener) {
	http.Serve(ln, s)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 阶段E：资源服务 /res/* —— 向宿主/插件 iframe 暴露共享资源（OCTools 可随意调用的等价物）
	if !websocket.IsWebSocketUpgrade(r) && strings.HasPrefix(r.URL.Path, "/res/") {
		s.serveRes(w, r)
		return
	}
	// 阶段G：插件 UI /plugin/<id>/* —— 向宿主 iframe 暴露插件自带 HTML 界面
	if !websocket.IsWebSocketUpgrade(r) && strings.HasPrefix(r.URL.Path, "/plugin/") {
		s.servePluginUI(w, r)
		return
	}
	// 新 BrowserWindow 会请求 /favicon.ico：返回应用 SVG 图标，避免 400/404 噪音。
	if !websocket.IsWebSocketUpgrade(r) && (r.URL.Path == "/favicon.ico" || r.URL.Path == "/favicon.svg") {
		s.serveFavicon(w, r)
		return
	}
	// 非 WebSocket 的普通 HTTP 直接 404，避免误交给 upgrader 返回 "Bad Request"。
	if !websocket.IsWebSocketUpgrade(r) {
		http.NotFound(w, r)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade: %v", err)
		return
	}
	// 多连接模型：宿主 + 各插件 iframe 各自独立持有一条 WS，互不互踢。
	log.Printf("[ws] connected from %s", r.RemoteAddr)
	go s.handle(conn)
}

// handle 首个消息必须是带 token 的 kernel.hello（D8），否则关闭。
func (s *Server) handle(conn *websocket.Conn) {
	defer conn.Close()
	// 首个消息
	_, raw, err := conn.ReadMessage()
	if err != nil {
		log.Printf("[ws] first msg read: %v", err)
		return
	}
	var hello protocol.Request
	if err := json.Unmarshal(raw, &hello); err != nil || hello.Method != "kernel.hello" {
		log.Printf("[ws] auth rejected: err=%v raw=%s", err, string(raw))
		return
	}
	var p struct {
		Token  string `json:"token"`
		Client string `json:"client"`
	}
	_ = json.Unmarshal(hello.Params, &p)
	if p.Token != s.authToken {
		log.Printf("[ws] auth rejected: bad token")
		return
	}
	// 仅宿主连接作为事件广播目标（plugin→宿主 event 推送）；插件 iframe 各自独立并发。
	if p.Client == HostClientName {
		s.setHost(conn)
	}
	s.reply(conn, protocol.NewResult(hello.ID, map[string]any{
		"kernel": "kerneld", "protocolVersion": protocol.ProtocolVersion,
		"capabilities": []string{"plugin", "stdio"},
	}))

	// 后续消息循环
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[ws] closed: %v", err)
			return
		}
		var req protocol.Request
		if err := json.Unmarshal(raw, &req); err != nil {
			s.reply(conn, protocol.NewError(0, protocol.ErrParse, nil))
			continue
		}
		// 每条消息独立 goroutine 执行：阻塞型 handler（如 handleCall 同步等待插件
		// 长耗时响应，超时可达 600s）不得堵住本连接的消息循环，否则后续 RPC
		// （如 renderer 600ms 轮询 plugin.list）全部排队超时。
		// reply/broadcast 均以 writeMu 串行化写入，并发回复安全。
		go s.dispatch(conn, req)
	}
}

func (s *Server) dispatch(conn *websocket.Conn, req protocol.Request) {
	switch req.Method {
	case "kernel.ping":
		s.reply(conn, protocol.NewResult(req.ID, map[string]any{}))
	case "plugin.list":
		s.handleList(conn, req)
	case "plugin.start":
		s.handlePluginStart(conn, req)
	case "plugin.call":
		s.handleCall(conn, req)
	case "plugin.details":
		s.handlePluginDetails(conn, req)
	case "plugin.restart":
		s.handlePluginRestart(conn, req)
	case "plugin.import":
		s.handlePluginImport(conn, req)
	case "plugin.remove":
		s.handlePluginRemove(conn, req)
	case "perms.list":
		s.handlePermsList(conn, req)
	case "perms.authorize":
		s.handlePermsAuthorize(conn, req)
	case "perms.revoke":
		s.handlePermsRevoke(conn, req)
	case "deps.preview", "runtime.preview":
		s.handleDepsPreview(conn, req)
	case "deps.install", "runtime.install":
		s.handleDepsInstall(conn, req)
	case "deps.getConfig", "runtime.getConfig":
		s.handleDepsGetConfig(conn, req)
	case "deps.setConfig", "runtime.setConfig":
		s.handleDepsSetConfig(conn, req)
	case "deps.envs", "runtime.envs":
		s.handleDepsEnvs(conn, req)
	case "registry.list":
		s.handleRegistryList(conn, req)
	case "registry.call":
		s.handleRegistryCall(conn, req)
	case "command.list":
		s.handleCommandList(conn, req)
	case "plugin.getLifecycle":
		s.handlePluginGetLifecycle(conn, req)
	case "plugin.getSettings":
		s.handlePluginGetSettings(conn, req)
	case "plugin.setSettings":
		s.handlePluginSetSettings(conn, req)
	case "plugin.updateSettings":
		s.handlePluginUpdateSettings(conn, req)
	case "plugin.resources":
		s.handlePluginResources(conn, req) // §17.3：读插件的资源设置（models.json 由内核读 state/）
	case "plugin.setResources":
		s.handlePluginSetResources(conn, req) // §17.3：写插件的资源设置（内核写 state/，宿主不直连）
	case "hotkeys.get":
		s.handleHotkeysGet(conn, req) // §17.3：读 state/hotkeys.json（内核读写，宿主不直连）
	case "hotkeys.set":
		s.handleHotkeysSet(conn, req)
	case "ui.getSettings":
		s.handleUISettingsGet(conn, req) // §17.2 B：宿主级 UI 设置（user-settings.json -> ui）
	case "ui.setSettings":
		s.handleUISettingsSet(conn, req)
	default:
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrMethodNotFound, nil))
	}
}

func (s *Server) handleList(conn *websocket.Conn, req protocol.Request) {
	sums := s.smanager.All()
	res := make([]map[string]any, 0, len(sums))
	for _, sm := range sums {
		res = append(res, map[string]any{
			"pluginId": sm.ID, "name": sm.Name, "type": sm.Type,
			"kind": sm.Kind, "state": sm.State, "disabled": sm.Disabled, "ui": sm.UI,
		})
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"plugins": res}))
}

// plugin.details 返回插件的完整描述（manifest 字段），供宿主渲染权限/命令等。
func (s *Server) handlePluginDetails(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	mf, ok := s.smanager.Describe(p.PluginID)
	if !ok {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrPluginMissing, map[string]any{"pluginId": p.PluginID}))
		return
	}
	decl, granted, high := s.gate.Subset(mf.ID)
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{
		"pluginId": mf.ID, "name": mf.Name, "type": mf.Type, "entry": mf.Entry,
		"kind": mf.Kind, "loadMode": mf.LoadMode, "uiMode": mf.UIMode, "ui": mf.UI,
		"permissionsDeclared": decl, "permissionsGranted": granted,
		"highRisk":     high,
		"dependencies": mf.Dependencies,
		"commands":     mf.Commands,
		"lifecycle":    mf.LifecyclePolicy,
		"state":        s.smanager.State(p.PluginID),
	}))
}

// plugin.getLifecycle 返回某插件的生命周期策略视图（默认/覆盖/生效）+ 状态。
func (s *Server) handlePluginGetLifecycle(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.PluginID == "" {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	view := s.smanager.Lifecycle(p.PluginID)
	s.reply(conn, protocol.NewResult(req.ID, view))
}

// plugin.getSettings 返回某插件的设置视图（§17.2 A）：
// settingsSchema（manifest 声明）+ user 设置（config/user-settings.json）+ 作者 defaults。
func (s *Server) handlePluginGetSettings(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	mf, ok := s.smanager.Describe(p.PluginID)
	if !ok {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrPluginMissing, map[string]any{"pluginId": p.PluginID}))
		return
	}
	var user map[string]any
	if s.settings != nil {
		if f, err := s.settings.Load(); err == nil {
			if pc, ok := f.Plugins[p.PluginID]; ok {
				user = pc.Settings
			}
		}
	}
	// 合并出生效设置（§5.3 冲突消解：user-settings → defaults → 内置默认）。
	ep := config.ResolvePlugin(p.PluginID, mf, config.PluginCfg{Settings: user, Profile: ""})
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{
		"pluginId": p.PluginID,
		"schema":   mf.SettingsSchema, // 原始 JSON Schema，宿主渲染表单（§7.4）
		"user":     user,
		"defaults": mf.Defaults,
		"effective": map[string]any{
			"profile": ep.Profile, "settings": ep.Settings,
		},
	}))
}

// plugin.setSettings 写插件内部设置（§9 plugins[id].settings；C 层唯一人工可写文件）。
func (s *Server) handlePluginSetSettings(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string         `json:"pluginId"`
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.PluginID == "" {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	if s.settings == nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": "settings store not configured"}))
		return
	}
	// §17.2 A MUST：settingsSchema 未覆盖的字段不得写入（丢弃未知键）。
	mf, ok := s.smanager.Describe(p.PluginID)
	allowed := settingsSchemaProps(mf.SettingsSchema)
	if ok && len(allowed) > 0 {
		filtered := map[string]any{}
		for _, k := range allowed {
			if v, has := p.Settings[k]; has {
				filtered[k] = v
			}
		}
		p.Settings = filtered
	}
	f, err := s.settings.Load()
	if err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": err.Error()}))
		return
	}
	if f.Plugins == nil {
		f.Plugins = map[string]config.PluginCfg{}
	}
	pc := f.Plugins[p.PluginID]
	pc.Settings = p.Settings
	f.Plugins[p.PluginID] = pc
	if err := s.settings.Save(f); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": err.Error()}))
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"ok": true}))
}

// settingsSchemaProps 提取 settingsSchema.properties 声明的字段名。
// 未声明 schema（nil/非对象/无 properties）时返回空集，调用方据此跳过过滤（保持向后兼容）。
func settingsSchemaProps(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil || len(schema.Properties) == 0 {
		return nil
	}
	keys := make([]string, 0, len(schema.Properties))
	for k := range schema.Properties {
		keys = append(keys, k)
	}
	return keys
}

// ui.getSettings 读宿主级 UI 设置（user-settings.json -> ui 节，如 sidebarOrder）。§17.2 B。
func (s *Server) handleUISettingsGet(conn *websocket.Conn, req protocol.Request) {
	ui := map[string]any{}
	if s.settings != nil {
		if f, err := s.settings.Load(); err == nil && f.UI != nil {
			for k, v := range f.UI {
				ui[k] = v
			}
		}
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"ui": ui}))
}

// ui.setSettings 写宿主级 UI 设置（user-settings.json -> ui 节）。§17.2 B。
func (s *Server) handleUISettingsSet(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		UI map[string]any `json:"ui"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.UI == nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	if s.settings == nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": "settings store not configured"}))
		return
	}
	f, err := s.settings.Load()
	if err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": err.Error()}))
		return
	}
	if f.UI == nil {
		f.UI = map[string]any{}
	}
	for k, v := range p.UI {
		f.UI[k] = v
	}
	if err := s.settings.Save(f); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": err.Error()}))
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"ok": true}))
}

// plugin.updateSettings 保存某插件的用户覆盖并立即生效（内部会重启该插件）。
// 参数与 Override 字段对齐；未提供/为 null 的字段视为不改动，清空传空对象 `{}` 意为恢复 manifest 默认。
func (s *Server) handlePluginUpdateSettings(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string             `json:"pluginId"`
		Override lifecycle.Override `json:"override"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.PluginID == "" {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	if err := s.smanager.SetLifecycle(p.PluginID, p.Override); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrDepsInstall, map[string]any{"error": err.Error()}))
		return
	}
	s.WireEvents() // 重启会新建 Plugin，需重注事件广播出口
	view := s.smanager.Lifecycle(p.PluginID)
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"ok": true, "effective": view.Effective, "state": view.State}))
}

// perms.list 查询某插件权限状态。
func (s *Server) handlePermsList(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	if s.gate == nil {
		s.reply(conn, protocol.NewResult(req.ID, map[string]any{}))
		return
	}
	decl, granted, _ := s.gate.Subset(p.PluginID)
	permsStatus := []map[string]any{}
	for _, d := range decl {
		permsStatus = append(permsStatus, map[string]any{
			"perm": d, "granted": contains(granted, d),
		})
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"pluginId": p.PluginID, "perms": permsStatus}))
}

// perms.authorize 首次授权（用户逐项或一键同意，FR-7）。
func (s *Server) handlePermsAuthorize(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string   `json:"pluginId"`
		Perms    []string `json:"perms"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.PluginID == "" {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	if err := s.gate.Authorize(p.PluginID, p.Perms); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrDepsInstall, map[string]any{"error": err.Error()}))
		return
	}
	_, granted, _ := s.gate.Subset(p.PluginID)
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"pluginId": p.PluginID, "granted": granted}))
}

// perms.revoke 动态收回某项权限（即时生效）。
func (s *Server) handlePermsRevoke(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
		Perm     string `json:"perm"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.PluginID == "" || p.Perm == "" {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	if err := s.gate.Revoke(p.PluginID, p.Perm); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrDepsInstall, map[string]any{"error": err.Error()}))
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"pluginId": p.PluginID, "revoked": p.Perm}))
}

// plugin.restart 重启指定插件（安装隔离依赖后切换解释器）。
func (s *Server) handlePluginRestart(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	if err := s.smanager.Restart(p.PluginID); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrPluginMissing, map[string]any{"error": err.Error()}))
		return
	}
	s.WireEvents() // 阶段E：重启会新建 Plugin，需重注事件广播出口
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"pluginId": p.PluginID, "restarted": true}))
}

// plugin.start 显式拉起插件进程（懒启动入口：前端点开未运行插件时调用，避免“加载 UI”隐式启动）。
func (s *Server) handlePluginStart(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	if _, err := s.smanager.GetOrStart(p.PluginID); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrPluginDown, map[string]any{"error": err.Error()}))
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"pluginId": p.PluginID, "started": true}))
}

// plugin.import 从源目录导入 OCTplugin 格式插件（复制进 plugins/<id> 并启动）。
func (s *Server) handlePluginImport(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		SrcPath string `json:"srcPath"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.SrcPath == "" {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	if err := s.smanager.Import(p.SrcPath); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrDepsInstall, map[string]any{"error": err.Error()}))
		return
	}
	s.WireEvents()
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"imported": true}))
}

// plugin.remove 永久移除插件（停进程 + 删目录）。
func (s *Server) handlePluginRemove(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.PluginID == "" {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	if err := s.smanager.Remove(p.PluginID); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrPluginMissing, map[string]any{"pluginId": p.PluginID, "error": err.Error()}))
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"pluginId": p.PluginID, "removed": true}))
}

// runtime.preview 返回待安装的依赖清单，供宿主弹窗确认（FR-3）。
func (s *Server) handleDepsPreview(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	// 依赖安装作用于「已登记」插件即可，不必要求其正在运行（lazy/prewarm 插件常未启动）。
	mf, ok := s.smanager.Describe(p.PluginID)
	if !ok || s.installer == nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrPluginMissing, map[string]any{"pluginId": p.PluginID}))
		return
	}
	rows := s.installer.Preview(mf)
	// 是否已就绪（Python 做顶层包导入探测，Node 看插件目录 node_modules）
	satisfied := s.installer.Ready(mf)
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{
		"pluginId": p.PluginID, "dependencies": rows, "satisfied": satisfied,
	}))
}

// runtime.install 安装插件隔离依赖（uv venv + pip/rpm），失败自动回滚。
func (s *Server) handleDepsInstall(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
		Force    bool   `json:"force"`
	}
	_ = json.Unmarshal(req.Params, &p)
	// 依赖安装作用于「已登记」插件即可，不必要求其正在运行（lazy/prewarm 插件常未启动）。
	mf, ok := s.smanager.Describe(p.PluginID)
	if !ok || s.installer == nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrPluginMissing, map[string]any{"pluginId": p.PluginID}))
		return
	}
	// 已就绪则幂等返回（Python 做顶层包导入探测，Node 看插件目录里的 node_modules）。
	// force=true 时跳过该短路，无论 venv 是否存在都按 manifest 依赖重新安装，
	// 用于 requirements.txt 等新增依赖后触发的“重装生效”。
	satisfied := s.installer.Ready(mf)
	if satisfied && !p.Force {
		s.reply(conn, protocol.NewResult(req.ID, map[string]any{
			"pluginId": p.PluginID, "installed": true, "restarted": false,
			"satisfied": true, "venvPython": s.installer.VenvPython(mf),
		}))
		return
	}
	// 首次安装：先停掉持有该 venv/目录的插件进程，否则 Windows 无法删除依赖目录（Access denied）
	_ = s.smanager.StopPlugin(p.PluginID)
	if err := s.installer.Install(mf, func(phase string) {
		// §6.4 MUST：安装过程向 UI 广播进度
		s.broadcast("kernel", "plugin.deps.progress", map[string]any{"id": p.PluginID, "phase": phase})
	}); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrDepsInstall, map[string]any{"error": err.Error()}))
		return
	}
	// 安装后会重启插件（全新进程，解释器经 interp 重新解析；事件出口需重注）
	if rerr := s.smanager.Restart(p.PluginID); rerr != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrDepsInstall, map[string]any{"error": "restart after install: " + rerr.Error()}))
		return
	}
	s.WireEvents()
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{
		"pluginId": p.PluginID, "installed": true, "restarted": true,
		"satisfied": true, "venvPython": s.installer.VenvPython(mf),
	}))
}

// runtime.getConfig 读取当前依赖源设置（镜像源/缓存目录）。
func (s *Server) handleDepsGetConfig(conn *websocket.Conn, req protocol.Request) {
	if s.installer == nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrMethodNotFound, nil))
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{
		"index_urls": s.installer.IndexURLs, "cache_dir": s.installer.CacheDir,
	}))
}

// runtime.setConfig 更新依赖源设置并持久化到 state/runtime.json（下次安装立即生效）。
func (s *Server) handleDepsSetConfig(conn *websocket.Conn, req protocol.Request) {
	if s.installer == nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrMethodNotFound, nil))
		return
	}
	var p struct {
		IndexURLs []string `json:"index_urls"`
		CacheDir  string   `json:"cache_dir"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	s.installer.SetIndex(p.IndexURLs).SetCacheDir(p.CacheDir)
	if err := s.installer.SaveConfig(); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": err.Error()}))
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"saved": true}))
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// runtime.envs 汇总已安装库及版本：项目托管 Python、各插件隔离 venv、Electron 宿主 Node 依赖。
func (s *Server) handleDepsEnvs(conn *websocket.Conn, req protocol.Request) {
	result := make(map[string]any)

	// 1) 项目托管 Python（uv managed 3.12）
	if mp, err := s.installer.ManagedPython(); err == nil {
		entry := map[string]any{"python": mp}
		if pkgs, e2 := s.installer.ListPackages(mp); e2 == nil {
			entry["packages"] = pkgs
		} else {
			entry["error"] = e2.Error()
		}
		result["managed"] = entry
	}

	// 2) 各插件隔离 venv（node 插件无 venv 自然被跳过）
	var plugins []map[string]any
	for _, sum := range s.smanager.All() {
		mf := sum.Manifest
		vp := s.installer.VenvPython(mf)
		if vp == "" {
			continue
		}
		entry := map[string]any{"pluginId": mf.ID, "name": mf.Name, "python": vp, "packages": []runtime.Pkg{}}
		if pkgs, e := s.installer.ListPackages(vp); e == nil {
			entry["packages"] = pkgs
		} else {
			entry["error"] = e.Error()
		}
		plugins = append(plugins, entry)
	}
	result["plugins"] = plugins

	// 3) Electron 宿主顶层 Node 依赖
	if pkgs, e := s.installer.NodePackages(); e == nil {
		result["node"] = map[string]any{"packages": pkgs}
	} else {
		result["node"] = map[string]any{"error": e.Error()}
	}

	s.reply(conn, protocol.NewResult(req.ID, result))
}

func (s *Server) handleCall(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string          `json:"pluginId"`
		Method   string          `json:"method"`
		Params   json.RawMessage `json:"params"`
		Timeout  *int            `json:"timeoutMs"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.PluginID == "" || p.Method == "" {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	// 懒启动：未运行（lazy/prewarm 或已回收）则先按需拉起，再派发请求。
	pl, gerr := s.smanager.GetOrStart(p.PluginID)
	if gerr != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrPluginDown, map[string]any{
			"pluginId": p.PluginID, "error": gerr.Error(),
		}))
		return
	}
	if !pl.Alive() {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrPluginDown, map[string]any{"pluginId": p.PluginID}))
		return
	}
	timeout := 15 * time.Second
	if p.Timeout != nil {
		timeout = time.Duration(*p.Timeout) * time.Millisecond
	}
	// 未显式给超时时，用插件 manifest 声明的单请求超时（若有）。
	if p.Timeout == nil && pl.Manifest.Limits.RequestTimeoutMs > 0 {
		timeout = time.Duration(pl.Manifest.Limits.RequestTimeoutMs) * time.Millisecond
	}
	var params any
	if len(p.Params) > 0 {
		_ = json.Unmarshal(p.Params, &params)
	}
	resp, err := pl.Call(p.Method, params, timeout)
	if err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrTimeout, map[string]any{"error": err.Error()}))
		return
	}
	if resp.Error != nil {
		s.reply(conn, protocol.Response{V: protocol.ProtocolVersion, JSONRPC: "2.0",
			ID: req.ID, Error: resp.Error})
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"ok": true, "result": resp.Result}))
}

// ── §17.3：宿主不直连 state/ 文件，一律经内核 RPC ──────────────────────────
// 资源设置（模型加载路径/策略）落盘 state/plugins/<pid>/models.json（§5.1：用户数据
// 一律在 state/plugins/<id>/ 下，绝不写入插件包 plugins/<id>/store/）。

// stateJSON 读 state 下相对路径的 JSON 文件；不存在返回 “ 与 nil。
func (s *Server) stateJSON(rel string) (map[string]any, error) {
	if s.stateDir == "" {
		return nil, fmt.Errorf("state dir not configured")
	}
	b, err := os.ReadFile(filepath.Join(s.stateDir, rel))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var data map[string]any
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Server) writeStateJSON(rel string, data map[string]any) error {
	if s.stateDir == "" {
		return fmt.Errorf("state dir not configured")
	}
	path := filepath.Join(s.stateDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// plugin.resources 读某插件的资源设置（models.json）。返回 {resources}.
func (s *Server) handlePluginResources(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
	}
	_ = json.Unmarshal(req.Params, &p)
	if p.PluginID == "" {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	rel := filepath.Join("plugins", p.PluginID, "models.json")
	data, err := s.stateJSON(rel)
	if err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": err.Error()}))
		return
	}
	res, _ := data["resources"].(map[string]any)
	if res == nil {
		res = map[string]any{}
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"pluginId": p.PluginID, "resources": res}))
}

// plugin.setResources 写某插件的一条资源设置（models.json）。参数 {key,path,load,unload,idle_min}.
func (s *Server) handlePluginSetResources(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		PluginID string `json:"pluginId"`
		Item     struct {
			Key     string `json:"key"`
			Path    string `json:"path"`
			Load    string `json:"load"`
			Unload  string `json:"unload"`
			IdleMin any    `json:"idle_min"`
		} `json:"item"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.PluginID == "" || p.Item.Key == "" {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	rel := filepath.Join("plugins", p.PluginID, "models.json")
	data, err := s.stateJSON(rel)
	if err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": err.Error()}))
		return
	}
	res, _ := data["resources"].(map[string]any)
	if res == nil {
		res = map[string]any{}
	}
	rec := map[string]any{"path": p.Item.Path}
	if p.Item.Load != "" {
		rec["load"] = p.Item.Load
	}
	if p.Item.Unload != "" {
		rec["unload"] = p.Item.Unload
	}
	if p.Item.IdleMin != nil {
		rec["idle_min"] = p.Item.IdleMin
	}
	res[p.Item.Key] = rec
	data["resources"] = res
	if err := s.writeStateJSON(rel, data); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": err.Error()}))
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"pluginId": p.PluginID, "ok": true}))
}

// hotkeys.get 读 state/hotkeys.json（host 经 RPC，而非直连文件）。
func (s *Server) handleHotkeysGet(conn *websocket.Conn, req protocol.Request) {
	data, err := s.stateJSON("hotkeys.json")
	if err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": err.Error()}))
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, data))
}

// hotkeys.set 写 state/hotkeys.json（host 经 RPC，而非直连文件）。
func (s *Server) handleHotkeysSet(conn *websocket.Conn, req protocol.Request) {
	var items map[string]any
	_ = json.Unmarshal(req.Params, &items)
	data, err := s.stateJSON("hotkeys.json")
	if err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": err.Error()}))
		return
	}
	// 仅改写 items 子节，保留 version 等其余字段。
	it, _ := data["items"].(map[string]any)
	if it == nil {
		it = map[string]any{}
	}
	for k, v := range items {
		it[k] = v
	}
	data["items"] = it
	if err := s.writeStateJSON("hotkeys.json", data); err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrIO, map[string]any{"error": err.Error()}))
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"ok": true}))
}

// registry.list 返回全部已注册共享函数（FR-8 注册表）。
func (s *Server) handleRegistryList(conn *websocket.Conn, req protocol.Request) {
	funcs := []map[string]any{}
	for _, f := range s.smanager.FuncList() {
		funcs = append(funcs, map[string]any{
			"name": f.Name, "method": f.Method, "desc": f.Desc, "pluginId": f.PluginID,
		})
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"functions": funcs}))
}

// registry.call 宿主按共享函数名调用（内核路由到所属插件）。
func (s *Server) handleRegistryCall(conn *websocket.Conn, req protocol.Request) {
	var p struct {
		Name    string          `json:"name"`
		Params  json.RawMessage `json:"params"`
		Timeout *int            `json:"timeoutMs"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrParse, nil))
		return
	}
	timeout := 15 * time.Second
	if p.Timeout != nil {
		timeout = time.Duration(*p.Timeout) * time.Millisecond
	}
	var params any
	if len(p.Params) > 0 {
		_ = json.Unmarshal(p.Params, &params)
	}
	resp, err := s.smanager.CallFunc(p.Name, params, timeout)
	if err != nil {
		s.reply(conn, protocol.NewError(req.ID, protocol.ErrPluginMissing, map[string]any{"error": err.Error()}))
		return
	}
	if resp.Error != nil {
		s.reply(conn, protocol.Response{V: protocol.ProtocolVersion, JSONRPC: "2.0",
			ID: req.ID, Error: resp.Error})
		return
	}
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"ok": true, "result": resp.Result}))
}

// serveRes 提供共享资源：/res/<相对路径> 映射到 resourcesDir，做目录穿越防护。
func (s *Server) serveRes(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/res/")
	clean := filepath.Clean("/" + rel) // 归一，阻止 ../ 逃逸
	target := filepath.Join(s.resourcesDir, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	if !strings.HasPrefix(target+sResourcesSep, s.resourcesDir+sResourcesSep) &&
		target != s.resourcesDir {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	st, err := os.Stat(target)
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, target)
}

const sResourcesSep = string(os.PathSeparator)

// servePluginUI 提供插件 iframe 界面静态文件：/plugin/<id>/<rel> → plugins/<id>/<rel>。
// 与 serveRes 同款目录穿越防护；仅服务声明 ui.type=web 的插件。
func (s *Server) servePluginUI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/plugin/") // "<id>[/<relpath>]"
	slash := strings.Index(rest, "/")
	if slash < 0 {
		http.NotFound(w, r)
		return
	}
	id, rel := rest[:slash], rest[slash+1:]
	// UI 是静态资源，不应要求插件进程已运行：lazy/prewarm 未启动时也需能 serve。
	mf, ok := s.smanager.Describe(id)
	if !ok || mf.UI.Type == "" {
		http.NotFound(w, r)
		return
	}
	pluginRoot := filepath.Join(s.pluginsDir, id)
	// 说明：加载 UI 不隐式启动进程。启动改由宿主前端显式 plugin.start 触发（避免 iframe 预载所有插件时把 lazy 全拉起）。
	lm := s.smanager.Lifecycle(id).Effective.LoadMode
	// disabled 插件：不可被 HTTP 触发启动，也不 serve 正常可交互 UI，返回“已禁用”状态页。
	if lm == lifecycle.LoadModeDisabled {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, disabledPaneHTML, id)
		return
	}
	target := filepath.Join(pluginRoot, filepath.FromSlash(filepath.Clean("/"+rel))) // 归一防穿越
	if !strings.HasPrefix(target+sResourcesSep, pluginRoot+sResourcesSep) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	f, err := os.Open(target)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

// serveFavicon 返回应用 logo（logo128.png）作为新 BrowserWindow 的站点图标。
func (s *Server) serveFavicon(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(filepath.Join(s.resourcesDir, "logo", "logo128.png"))
	if err != nil {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusNoContent) // 无图标文件时返回 204，避免 400/404 噪音
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/png")
	http.ServeContent(w, r, "logo128.png", time.Time{}, f)
}

// command.list 返回全部可执行命令（FR-9 命令面板），宿主据此补全。
func (s *Server) handleCommandList(conn *websocket.Conn, req protocol.Request) {
	s.reply(conn, protocol.NewResult(req.ID, map[string]any{"commands": s.smanager.Commands()}))
}

func (s *Server) reply(conn *websocket.Conn, resp protocol.Response) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := conn.WriteJSON(resp); err != nil {
		log.Printf("[ws] write: %v", err)
	}
}

// disabledPaneHTML 返回给“已禁用”插件的占位页：不承载可交互 UI，明确告知用户服务已停。
const disabledPaneHTML = `<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>body{margin:0;min-height:100vh;display:flex;flex-direction:column;align-items:center;justify-content:center;
font-family:system-ui,sans-serif;background:#f3ecdd;color:#7a7468;gap:10px}
.b{font-size:30px}.t{font-size:15px;font-weight:600;color:#1e1b17}
.s{font-size:12px;color:#8a8377;text-align:center;padding:0 30px}</style></head>
<body><div class="b">i</div><div class="t">该插件服务已禁用</div>
<div class="s">插件「%s」已设为 disabled，其进程不会启动，页面仅为占位。
请在 设置 → 启动与资源 中重新启用。</div></body></html>`
