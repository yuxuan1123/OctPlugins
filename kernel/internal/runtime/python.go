package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/octplugin/kernel/internal/lifecycle"
)

// pythonProvider 处理 Python 插件依赖：uv venv 隔离 + uv pip 安装（§6.5 表）。
type pythonProvider struct{ inst *Installer }

func (pythonProvider) Lang() string { return "python" }
func (pythonProvider) IsSupported(mf lifecycle.Manifest) bool {
	return isPythonType(mf.Type)
}

// Ready 判定 Python 插件依赖已就绪（隔离 venv 存在，或未声明依赖）。
func (p pythonProvider) Ready(mf lifecycle.Manifest) bool {
	return p.inst.RequirementsSatisfied(mf)
}

// GenerateLock Python 锁文件：uv pip compile requirements.txt → lockPath（§6.4）。
func (p pythonProvider) GenerateLock(_ context.Context, pluginDir, lockPath string) error {
	req := filepath.Join(pluginDir, "requirements.txt")
	if _, err := os.Stat(req); err != nil {
		return nil // 无 requirements.txt 则无锁可生成
	}
	cmd := exec.Command(p.inst.uvBin, "pip", "compile", req, "--quiet", "--output-file", lockPath)
	cmd.Dir = pluginDir
	if env := uvEnv(p.inst.CacheDir, p.inst.IndexURLs); len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("uv pip compile: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Install Python：创建隔离 venv 并把 requirements.txt 装进去（§6.4 后台安装）。
// 幂等：venv 已建**且依赖均已安装**才短路；空壳/半装 venv（如精简分发包只带目录、
// 或上次安装中断）会被判未就绪并补装，而不是误判 ready 后带着残缺环境启动。
func (p pythonProvider) Install(_ context.Context, mf lifecycle.Manifest, progress func(string)) error {
	venv := filepath.Join(p.inst.depsRoot(mf.ID), "venv")
	if p.inst.venvDepsReady(mf) {
		progress("ready")
		return nil // 已就绪
	}
	progress("creating-venv")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		return err
	}
	// venv 缺失/损坏时才重建（已有 venv 直接复用，只补装 requirements）。
	if !fileExists(p.inst.venvPythonPath(mf.ID)) {
		if err := p.inst.runUV(mf.Dir, p.inst.venvArgs(mf, venv)...); err != nil {
			return fmt.Errorf("create venv: %w", err)
		}
	}
	progress("installing")
	if err := p.inst.pipInstall(mf, venv); err != nil {
		return err
	}
	progress("ready")
	return nil
}

// venvArgs 组装 `uv venv` 参数：优先使用随包分发的托管解释器（root/runtime/python，
// 由打包脚本放入），退回让 uv 按 manifest.python_version（默认 3.12）自行解析/下载。
// 显式指定版本可避免全新机器上挑到系统里不兼容的 Python。
func (i *Installer) venvArgs(mf lifecycle.Manifest, venv string) []string {
	if bp := i.BundledPython(); bp != "" {
		return []string{"venv", "--python", bp, venv}
	}
	ver := strings.TrimSpace(mf.Py3v)
	if ver == "" {
		ver = "3.12"
	}
	return []string{"venv", "--python", ver, venv}
}

// BundledPythonDir 返回随包托管解释器根目录（root/runtime/python）。打包脚本把
// uv 托管 CPython 放进这里后，内核据此设置 UV_PYTHON_INSTALL_DIR，使 uv 解析解释器
// 与新建 venv 都落在包内，而非用户 AppData（换机器仍需联网下载）。
func (i *Installer) BundledPythonDir() string {
	return filepath.Join(i.root, "runtime", "python")
}

// BundledPython 返回随包分发的托管解释器路径（root/runtime/python/<name>/python.exe）；
// 未随包分发时返回空串。
func (i *Installer) BundledPython() string {
	root := i.BundledPythonDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "cpython-") {
			continue
		}
		if p := filepath.Join(root, e.Name(), pythonExe()); fileExists(p) {
			return p
		}
	}
	return ""
}

// -------- 以下为 Installer 的 Python 门面方法（python.go 内实现，维持对外签名不变） --------

func (i *Installer) depsRoot(id string) string {
	return filepath.Join(i.root, "runtime", id)
}

func (i *Installer) venvPythonPath(id string) string {
	binDir := "bin"
	if runtime.GOOS == "windows" {
		binDir = "Scripts"
	}
	return filepath.Join(i.depsRoot(id), "venv", binDir, pythonExe())
}

// isPythonType 判断插件类型是否为 Python。
func isPythonType(t string) bool {
	lt := strings.ToLower(t)
	return !strings.Contains(lt, "node") && !strings.Contains(lt, "js") &&
		!strings.Contains(lt, "go") && !strings.Contains(lt, "rust") && !strings.Contains(lt, "cargo")
}

func pythonExe() string {
	if runtime.GOOS == "windows" {
		return "python.exe"
	}
	return "python"
}

// VenvPython 返回某插件的隔离解释器路径；无 venv 或非 Python 插件返回空串。
func (i *Installer) VenvPython(mf lifecycle.Manifest) string {
	if !isPythonType(mf.Type) {
		return ""
	}
	p := i.venvPythonPath(mf.ID)
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

// RequirementsSatisfied 判定某 Python 插件依赖是否已就绪。
// 未声明 requirements 视为就绪；否则要求隔离 venv 存在**且 requirements.txt 声明的包
// 都有对应 *.dist-info**（仅存在空壳 venv 不算就绪，§6.4）。
func (i *Installer) RequirementsSatisfied(mf lifecycle.Manifest) bool {
	if !isPythonType(mf.Type) {
		return false
	}
	hasReqs := hasRequirements(filepath.Join(mf.Dir, "requirements.txt")) ||
		hasRequirements(filepath.Join(mf.Dir, "pyproject.toml")) ||
		len(mf.Dependencies) > 0
	if !hasReqs {
		return true
	}
	return i.venvDepsReady(mf)
}

// venvDepsReady 判定插件隔离 venv 是否真的装好了 requirements.txt 的依赖。
// 判定依据是 site-packages 下的 *.dist-info（而非「venv 目录存在」）——
// 精简分发包只带空壳/不带 venv，或安装中断，都必须判未就绪以触发安装。
func (i *Installer) venvDepsReady(mf lifecycle.Manifest) bool {
	if !fileExists(i.venvPythonPath(mf.ID)) {
		return false
	}
	names := requirementNames(filepath.Join(mf.Dir, "requirements.txt"))
	if len(names) == 0 {
		return true // 无 requirements 声明：解释器在位即视为就绪
	}
	installed := installedDistNames(filepath.Join(i.depsRoot(mf.ID), "venv"))
	for _, n := range names {
		if !installed[normalizePkg(n)] {
			return false
		}
	}
	return true
}

// requirementNames 解析 requirements.txt，返回顶层包名（去版本约束/扩展/环境标记/注释；
// 跳过空行与 -r/-e/--index-url 等选项行）。
func requirementNames(reqPath string) []string {
	b, err := os.ReadFile(reqPath)
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}
		if k := strings.IndexByte(line, '#'); k >= 0 { // 行尾注释
			line = strings.TrimSpace(line[:k])
		}
		if k := strings.IndexByte(line, ';'); k >= 0 { // 环境标记
			line = strings.TrimSpace(line[:k])
		}
		if k := strings.IndexAny(line, "=<>!~[ \t"); k > 0 {
			line = line[:k]
		}
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

// installedDistNames 返回 venv 的 site-packages 中已安装包名集合（PEP 503 归一化）。
func installedDistNames(venv string) map[string]bool {
	out := map[string]bool{}
	site := venvSitePackages(venv)
	if site == "" {
		return out
	}
	entries, err := os.ReadDir(site)
	if err != nil {
		return out
	}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() || !strings.HasSuffix(n, ".dist-info") {
			continue
		}
		base := strings.TrimSuffix(n, ".dist-info")
		if k := strings.LastIndex(base, "-"); k > 0 { // 去掉尾部 -<version>
			base = base[:k]
		}
		out[normalizePkg(base)] = true
	}
	return out
}

// venvSitePackages 返回 venv 的 site-packages 目录；不存在返回空串。
// Windows: venv/Lib/site-packages；Unix: venv/lib/python3.x/site-packages。
func venvSitePackages(venv string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Lib", "site-packages")
	}
	libDir := filepath.Join(venv, "lib")
	entries, err := os.ReadDir(libDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "python3") {
			return filepath.Join(libDir, e.Name(), "site-packages")
		}
	}
	return ""
}

// normalizePkg 归一化包名用于比较（PEP 503：小写，_/. 统一为 -）。
func normalizePkg(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	return strings.ReplaceAll(s, ".", "-")
}

// pipInstall 把 requirements.txt 装进插件隔离 venv。
func (i *Installer) pipInstall(mf lifecycle.Manifest, venv string) error {
	if mf.Dir == "" {
		return nil
	}
	req := filepath.Join(mf.Dir, "requirements.txt")
	if !hasRequirements(req) {
		return nil
	}
	py := filepath.Join(venv, pythonExe())
	if runtime.GOOS == "windows" {
		py = filepath.Join(venv, "Scripts", pythonExe())
	} else {
		py = filepath.Join(venv, "bin", pythonExe())
	}
	args := []string{"pip", "install", "--python", py}
	for _, u := range i.IndexURLs {
		args = append(args, "--index-url", u)
	}
	args = append(args, "-r", req)
	return i.runUV(mf.Dir, args...)
}

// ManagedPython 返回项目托管 Python（随包分发的 3.12，或 uv managed 3.12）。
// 优先用随包解释器；否则经 `uv python find` 定位；再退回 PATH 上的 python 候选。
func (i *Installer) ManagedPython() (string, error) {
	if p := i.BundledPython(); p != "" {
		return p, nil
	}
	if out, err := exec.Command(i.uvBin, "python", "find", "3.12").CombinedOutput(); err == nil {
		p := strings.TrimSpace(string(out))
		if p != "" && fileExists(p) {
			return p, nil
		}
	}
	for _, cand := range []string{"python3.12", "python3", "python"} {
		if p, err := exec.LookPath(cand); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("managed python not found (uv missing or no 3.12)")
}

// ListPackages 列出给定解释器环境中已安装的库（pip list --json）。
func (i *Installer) ListPackages(py string) ([]Pkg, error) {
	out, err := exec.Command(py, "-m", "pip", "list", "--format=json", "--disable-pip-version-check").CombinedOutput()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	pkgs := make([]Pkg, 0, len(raw))
	for _, r := range raw {
		pkgs = append(pkgs, Pkg{Name: r.Name, Version: r.Version})
	}
	return pkgs, nil
}
