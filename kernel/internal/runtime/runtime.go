// Package runtime 管理插件代码依赖（uv venv / pip / node_modules），《理想架构.md》§6。
//
// 本实现为骨架版：优先保证内核可编译、插件可加载、UI 可用。
// - uv 缺失时 VenvPython 回退系统 python；
// - Ready 保守返回 true（不阻塞加载）；
// - Install 仅占位（不真正执行 uv pip install）；
// 完整的 lockHash/后台安装链列为后续增强。
package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/octplugin/kernel/internal/lifecycle"
)

// Pkg 已安装包的描述（name + version），供 deps.envs 展示。
type Pkg struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Installer 依赖安装器。
type Installer struct {
	mu        sync.Mutex
	root      string
	uvBin     string
	cfgFile   string
	IndexURLs []string
	CacheDir  string
}

// New 创建安装器。uvBin 为空时回退 "uv"（PATH 查找）。
func New(root, uvBin string) *Installer {
	if uvBin == "" {
		uvBin = "uv"
	}
	return &Installer{root: root, uvBin: uvBin}
}

// UseConfigFile 指定依赖源配置文件路径（state/runtime.json）。
func (i *Installer) UseConfigFile(path string) {
	i.mu.Lock()
	i.cfgFile = path
	i.mu.Unlock()
}

// LoadConfig 读取 state/runtime.json（镜像源/缓存目录）。文件不存在不报错。
func (i *Installer) LoadConfig() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cfgFile == "" {
		return nil
	}
	b, err := os.ReadFile(i.cfgFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg struct {
		IndexURLs []string `json:"index_urls"`
		CacheDir  string   `json:"cache_dir"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	i.IndexURLs = cfg.IndexURLs
	i.CacheDir = cfg.CacheDir
	return nil
}

// SaveConfig 持久化镜像源/缓存目录到 state/runtime.json。
func (i *Installer) SaveConfig() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cfgFile == "" {
		return fmt.Errorf("runtime config file not set")
	}
	if err := os.MkdirAll(filepath.Dir(i.cfgFile), 0o755); err != nil {
		return err
	}
	cfg := map[string]any{
		"index_urls": i.IndexURLs,
		"cache_dir":  i.CacheDir,
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	tmp := i.cfgFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, i.cfgFile)
}

// SetIndex 设置 pip 镜像源（链式调用）。
func (i *Installer) SetIndex(urls []string) *Installer {
	i.mu.Lock()
	i.IndexURLs = urls
	i.mu.Unlock()
	return i
}

// SetCacheDir 设置 uv 缓存目录（链式调用）。
func (i *Installer) SetCacheDir(dir string) *Installer {
	i.mu.Lock()
	i.CacheDir = dir
	i.mu.Unlock()
	return i
}

// venvLaunchCandidates 返回某个 venv 目录下解释器的候选路径（Windows Scripts / Unix bin）。
func venvLaunchCandidates(venvDir string) []string {
	return []string{
		filepath.Join(venvDir, "Scripts", "python.exe"),
		filepath.Join(venvDir, "bin", "python3"),
		filepath.Join(venvDir, "bin", "python"),
	}
}

// VenvPython 返回插件使用的 Python 解释器路径。查找顺序：
//  1. <pluginDir>/.venv（插件包自带环境 / 开发态）
//  2. <root>/runtime/<pluginId>/venv（package.ps1 随包分发的隔离环境）
//  3. 随包分发的托管 CPython（<root>/runtime/python/<impl>）
//  4. 系统 python（PATH）
func (i *Installer) VenvPython(mf lifecycle.Manifest) string {
	for _, venvDir := range []string{
		filepath.Join(mf.Dir, ".venv"),
		filepath.Join(i.root, "runtime", mf.ID, "venv"),
	} {
		for _, c := range venvLaunchCandidates(venvDir) {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c
			}
		}
	}
	// 无可用 venv → 用随包解释器，最后回退系统 python
	if p := i.BundledPython(); p != "" {
		return p
	}
	if p, err := exec.LookPath("python"); err == nil {
		return p
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	return "python"
}

// Ready 依赖是否就绪。骨架版保守返回 true（不阻塞加载）。
func (i *Installer) Ready(mf lifecycle.Manifest) bool {
	return true
}

// NodeInterp 返回 Node 插件解释器路径及是否找到。
func (i *Installer) NodeInterp(mf lifecycle.Manifest) (string, bool) {
	// 仅 Node 类型插件使用 node 解释器；其它类型（Python/Go/…）不得命中，否则
	// 会被错误地当作 Node 执行（如用 node 去跑 main.py → SyntaxError）。
	if !isNode(mf.Type) {
		return "", false
	}
	// 随包分发的 node（package.ps1 会把 bin\node.exe 打进包）优先，其次 PATH。
	for _, c := range []string{
		filepath.Join(i.root, "bin", "node.exe"),
		filepath.Join(i.root, "bin", "node"),
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, true
		}
	}
	p, err := exec.LookPath("node")
	if err != nil {
		return "", false
	}
	return p, true
}

// DepsPlan 计算 jsonHash/lockHash/cacheDir（§6/§8）。骨架版返回空 hash 不阻塞。
func (i *Installer) DepsPlan(mf lifecycle.Manifest) (jsonHash, lockHash, cacheDir string, err error) {
	// jsonHash: manifest.json 的 codeDeps 子树 hash
	b, _ := json.Marshal(mf.CodeDeps)
	h := sha256.Sum256(b)
	jsonHash = hex.EncodeToString(h[:])
	// lockHash: 暂无 start.lock，用空 hash
	lockHash = ""
	// cacheDir: state/plugins/<id>/.cache
	cacheDir = filepath.Join(i.root, "state", "plugins", mf.ID, ".cache")
	return jsonHash, lockHash, cacheDir, nil
}

// Install 安装插件依赖。骨架版仅记录日志，不真正执行 uv pip install。
// 完整安装流（uv venv create + uv pip install -r requirements.txt）为后续增强。
func (i *Installer) Install(mf lifecycle.Manifest, onPhase func(phase string)) error {
	if onPhase != nil {
		onPhase("skipping (runtime skeleton)")
	}
	return nil
}

// Preview 返回待安装的依赖清单。骨架版返回空。
func (i *Installer) Preview(mf lifecycle.Manifest) []Pkg {
	return []Pkg{}
}

// BundledPythonDir 返回随包分发的托管解释器根目录（供 uv 的 UV_PYTHON_INSTALL_DIR 使用）：
// <root>/runtime/python（root 为内核项目根：开发态=仓库根，打包态=resources\）。
// 目录不存在返回空串（调用方不做注入，回落 uv 托管/系统解释器）。
func (i *Installer) BundledPythonDir() string {
	dir := filepath.Join(i.root, "runtime", "python")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ""
	}
	return dir
}

// BundledPython 返回随包分发的托管 Python 可执行文件路径；未随包分发时返回空串。
// 目录约定按 uv python install 的落盘布局：<dir>/<implementation>-<version>-<platform>/…
// （Windows 为 python.exe，Unix 为 bin/python3|python）。
func (i *Installer) BundledPython() string {
	dir := i.BundledPythonDir()
	if dir == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	rels := [][]string{{"python.exe"}, {"bin", "python3"}, {"bin", "python"}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, rel := range rels {
			p := filepath.Join(append([]string{dir, e.Name()}, rel...)...)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

// ManagedPython 返回项目托管 Python 路径（uv managed）。骨架版回退系统 python。
func (i *Installer) ManagedPython() (string, error) {
	if p, err := exec.LookPath("python"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("python not found")
}

// ListPackages 列出指定 python 环境的已安装包。骨架版返回空。
func (i *Installer) ListPackages(pythonPath string) ([]Pkg, error) {
	return []Pkg{}, nil
}

// NodePackages 列出 Electron 宿主的 Node 依赖。骨架版返回空。
func (i *Installer) NodePackages() ([]Pkg, error) {
	return []Pkg{}, nil
}

// 辅助：判断是否为 Node 类型插件
func isNode(t string) bool {
	lt := strings.ToLower(t)
	return strings.Contains(lt, "node") || strings.Contains(lt, "js") || strings.Contains(lt, "javascript")
}
