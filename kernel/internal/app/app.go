// Package app 是内核的装配根：依赖注入、信号处理、退出清理（§12.4 / §4 cmd/kerneld）。
//
// 对齐《理想架构.md》§2.1.3 / §12.4：
//   - 装配 manager / installer / ipcserver，输出 auth 首行交予宿主；
//   - signal.Notify(SIGINT, SIGTERM)：退出前对全部单元执行阶梯关闭并递归兜底，
//     确保无残留进程（Windows 由 Job Object KILL_ON_JOB_CLOSE 兜底，§13.1）。
package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/octplugin/kernel/internal/config"
	"github.com/octplugin/kernel/internal/ipcserver"
	"github.com/octplugin/kernel/internal/lifecycle"
	"github.com/octplugin/kernel/internal/perms"
	"github.com/octplugin/kernel/internal/registry"
	"github.com/octplugin/kernel/internal/resources"
	"github.com/octplugin/kernel/internal/runtime"
)

// Run 以内核二进制所在目录的上一级为项目根，装配并阻塞运行内核。
// 返回 nil 仅在信号退出清理完成后；此时进程应直接退出。
func Run(root string) error {
	// 内核 stdout 只用于输出 auth 行（§2.1.3），其余日志一律走 stderr。
	log.SetOutput(os.Stderr)

	pluginsDir := filepath.Join(root, "plugins")
	storeDir := filepath.Join(root, "state")
	resourcesDir := filepath.Join(root, "resources") // 阶段E：共享资源根目录

	// 授权持久化 store/perms.json（阶段A）
	gate := perms.NewGate(filepath.Join(storeDir, "perms.json"))

	manager := lifecycle.NewManager(pluginsDir, gate)
	// 阶段三：用户生命周期覆盖持久化 state/overrides.json
	manager.SetStoreDir(storeDir)
	// §8：registry.json 唯一写入方（内核独占写，原子写；插件条目 dataDir 不受 remove 影响）。
	manager.SetRegistryStore(registry.New(filepath.Join(storeDir, "registry.json")))
	// §20.8：state/secrets.json 以 0600 权限预置（插件仅持管道，不得接触凭据明文）。
	if err := ensureSecrets(filepath.Join(storeDir, "secrets.json")); err != nil {
		log.Printf("[kernel] ensure secrets.json: %v", err)
	}
	// 阶段B：依赖隔离——uv 从环境注入，失败回退 "uv"
	uvBin := os.Getenv("OCTRUN_UV")
	if uvBin == "" {
		uvBin = "uv"
	}
	installer := runtime.New(root, uvBin)
	// 随包分发的托管解释器（resources/runtime/python）：令 uv 以包内目录为解释器根，
	// 使 `uv venv` / `uv python find` 命中包内解释器，而不是用户 AppData（换机不丢）。
	if bp := installer.BundledPython(); bp != "" {
		_ = os.Setenv("UV_PYTHON_INSTALL_DIR", installer.BundledPythonDir())
		log.Printf("[kernel] bundled python: %s", bp)
	}
	// 依赖源设置优先级：UI 持久化配置(state/runtime.json) 为基础，环境变量临时覆盖。
	installer.UseConfigFile(filepath.Join(storeDir, "runtime.json"))
	if err := installer.LoadConfig(); err != nil {
		log.Printf("[kernel] load runtime.json: %v", err)
	}
	// 下载镜像源：OCTPY_INDEX 可多个，空格/逗号分隔，首个最优先
	if v := os.Getenv("OCTPY_INDEX"); v != "" {
		installer.SetIndex(splitEnvList(v))
	}
	// 自定义下载缓存目录：OCTUV_CACHE_DIR
	if v := os.Getenv("OCTUV_CACHE_DIR"); v != "" {
		installer.SetCacheDir(v)
	}
	manager.SetVenvResolver(installer.VenvPython)
	manager.SetDepsReadyResolver(installer.Ready) // 依赖就绪判定（Python venv / Node node_modules）导入探测
	manager.SetNodeResolver(installer.NodeInterp) // 阶段F：Node 插件用 node 解释器
	manager.SetDepsPlan(installer.DepsPlan)       // §6/§8：spawn 前锁判定并入 registry
	manager.SetInstaller(installer.Install)       // §6.4：依赖未命中时后台真实安装（含进度回调）再续启
	// §16.5：oct_sdk 注入路径（root/sdk/python），spawn 经 PYTHONPATH 使插件 import。
	manager.SetSdkPythonPath(filepath.Join(root, "sdk", "python"))
	// §14 ResourceMap：externalDependencies（工具/模型）的登记/获取/释放/状态落盘。
	resourceMap := resources.NewManager()
	manager.SetResourceManager(resourceMap)
	manager.SetResourcesDir(filepath.Join(resourcesDir, "tools")) // §14.2 bundled 定位根
	defer resourceMap.StopAll()                                   // §12.4 退出清理兜底

	token := newToken(32)
	srv := ipcserver.NewServer(token, manager, gate, installer, resourcesDir, pluginsDir)
	// §9/§17.2：user-settings.json 作为 settingsSchema 驱动设置页的唯一落点（C 层）。
	settingsStore := config.NewSettings(filepath.Join(root, "config", "user-settings.json"))
	srv.SetSettingsStore(settingsStore)
	// 转化域 §8.1：能力级 pin 的唯一读取口。lifecycle 不直接依赖 config（config 已依赖
	// lifecycle，直接引用会形成循环导入），故以 provider 形式注入。
	manager.SetPinsProvider(func() map[string]string {
		f, err := settingsStore.Load()
		if err != nil {
			log.Printf("[kernel] load pins: %v", err)
			return map[string]string{}
		}
		return f.Pins()
	})
	// 转化域 §8.1：pin 的唯一写入方是内核（落 config/user-settings.json）。
	manager.SetPinWriter(settingsStore.SetPin)
	// §17.2 A：插件「自己的」设置经装配根接 config.Settings + ResolvePlugin，
	// 与宿主「设置→插件/tool内部设置」页（plugin.getSettings/setSettings）同一合并语义。
	manager.SetSettingsResolver(func(pluginID string, mf lifecycle.Manifest) (map[string]any, error) {
		f, err := settingsStore.Load()
		if err != nil {
			return nil, err
		}
		user := map[string]any{}
		if pc, ok := f.Plugins[pluginID]; ok && pc.Settings != nil {
			user = pc.Settings
		}
		ep := config.ResolvePlugin(pluginID, mf, config.PluginCfg{Settings: user, Profile: ""})
		return ep.Settings, nil
	})
	manager.SetSettingsWriter(func(pluginID string, mf lifecycle.Manifest, settings map[string]any) error {
		// §17.2 A MUST：settingsSchema 未覆盖的字段不得写入（丢弃未知键）。
		allowed := config.SchemaProps(mf.SettingsSchema)
		filtered := map[string]any{}
		for _, k := range allowed {
			if v, has := settings[k]; has {
				filtered[k] = v
			}
		}
		f, err := settingsStore.Load()
		if err != nil {
			return err
		}
		if f.Plugins == nil {
			f.Plugins = map[string]config.PluginCfg{}
		}
		pc := f.Plugins[pluginID]
		pc.Settings = filtered
		f.Plugins[pluginID] = pc
		return settingsStore.Save(f)
	})
	// 转化域 §20.5：模型存储根由用户决定（user-settings.json 的 models.root），空则用内置默认。
	if f, err := settingsStore.Load(); err == nil {
		manager.SetModelsRoot(f.ModelsRoot())
	}
	// §17.3：内核独占写 state/。宿主一律经 RPC（plugin.resources/hotkeys.*）读写，不得直连文件。
	srv.SetStateDir(storeDir)

	ln, port, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// 事件广播/状态回调先装配好，供随后异步启动的插件实例使用。
	manager.SetOnState(func(id, state string) { srv.NotifyState(id, state) })
	manager.SetOnDepsProgress(func(id, phase string) { srv.NotifyDepsProgress(id, phase) })
	manager.SetOnModelEvent(func(typ string, data any) { srv.NotifyModelEvent(typ, data) })
	manager.SetOnSpawn(func(p *lifecycle.Plugin) { p.Event = srv.EventSink() })
	srv.WireEvents()
	defer manager.StopAll()

	// 同步快速登记全部插件：plugin.list / 侧栏「注册表预显示」在 auth 前即完整。
	manager.RegisterAll()
	// 转化域 §5.2/§5.3：outtool 描述清单里的模型声明同样登记进 registry.json.models
	// （outtool 无 manifest，故需独立清单承载；provider 记为 outtool id）。
	manager.RegisterOuttoolsFrom(resourcesDir)
	manager.SyncResourceRegistry() // §8：登记后把 externalDependencies 资源节落盘 registry.json

	go srv.Serve(ln)

	auth, _ := json.Marshal(map[string]any{
		"auth": map[string]any{"port": port, "token": token},
	})
	fmt.Println(string(auth))
	// 插件异步、逐插件并发启动：不阻塞 auth；各插件状态经 plugin.state 事件
	// 实时推送（灰色→启动→运行），单个插件依赖卡住不影响其他插件与整体 UI。
	go manager.StartAll()

	// §12.4：信号退出——先优雅关闭全部单元（manager.StopAll 由 defer 兜底），再退出。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("[kernel] signal received; shutting down")
	return nil
}

func splitEnvList(v string) []string {
	var urls []string
	for _, u := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	return urls
}

func newToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ensureSecrets 预置 state/secrets.json（0600，§20.8）。已存在则仅修正权限，不覆盖内容。
func ensureSecrets(path string) error {
	if _, err := os.Stat(path); err == nil {
		_ = os.Chmod(path, 0o600)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		return err
	}
	return nil
}
