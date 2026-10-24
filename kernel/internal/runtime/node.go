package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/octplugin/kernel/internal/lifecycle"
)

// nodeProvider 处理 Node 插件依赖：独立 node_modules + 系统 node 解释器（§6.5 表）。
// lockfile 语义：MVP 复用仓库既有 pnpm-lock.yaml；此处专注 §6.4 的安装 gating。
type nodeProvider struct{ inst *Installer }

func (nodeProvider) Lang() string { return "node" }
func (nodeProvider) IsSupported(mf lifecycle.Manifest) bool {
	return isNodeTypeType(mf.Type)
}

// Ready 判定 Node 插件依赖是否就绪（§6.4 gating）。
// 无 package.json、或 package.json 未声明依赖、或 node_modules 已存在 → 就绪。
func (nodeProvider) Ready(mf lifecycle.Manifest) bool {
	pkg := filepath.Join(mf.Dir, "package.json")
	if !hasRequirements(pkg) {
		return true
	}
	if hasRequirements(filepath.Join(mf.Dir, "node_modules")) {
		return true
	}
	var pj struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if b, err := os.ReadFile(pkg); err == nil && json.Unmarshal(b, &pj) == nil {
		if len(pj.Dependencies) == 0 && len(pj.DevDependencies) == 0 {
			return true // 声明了运行时但无第三方依赖，无需安装
		}
	}
	return false
}

// GenerateLock Node：MVP 复用仓库既有锁（pnpm-lock.yaml），不强制生成。
func (nodeProvider) GenerateLock(context.Context, string, string) error { return nil }

// Install Node：执行真实依赖安装到插件目录 node_modules（§6.4 后台安装）。
// 工具链优先 pnpm（§6.5 表），回退 npm。幂等：node_modules 已存在即返回 nil。
func (n nodeProvider) Install(_ context.Context, mf lifecycle.Manifest, progress func(string)) error {
	pkg := filepath.Join(mf.Dir, "package.json")
	if !hasRequirements(pkg) {
		progress("ready")
		return nil
	}
	if hasRequirements(filepath.Join(mf.Dir, "node_modules")) {
		progress("ready")
		return nil
	}
	bin, args, err := n.nodeTool()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = mf.Dir
	// pnpm 共享存储缓存（§6.5 表：PNPM_STORE）；独立 node_modules 隔离。
	if bin == "pnpm" && n.inst.CacheDir != "" {
		cmd.Env = append(os.Environ(), "PNPM_STORE="+n.inst.CacheDir)
	}
	progress("installing")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s install: %w: %s", bin, err, strings.TrimSpace(string(out)))
	}
	progress("ready")
	return nil
}

// nodeTool 决议安装工具：优先 pnpm，回退 npm。
func (n nodeProvider) nodeTool() (string, []string, error) {
	for _, cand := range []struct{ bin string; args []string }{
		{"pnpm", []string{"install", "--prod", "--ignore-scripts"}},
		{"npm", []string{"install", "--production", "--ignore-scripts"}},
	} {
		if p, err := exec.LookPath(cand.bin); err == nil {
			return p, cand.args, nil
		}
	}
	return "", nil, fmt.Errorf("no node package manager found (need pnpm or npm)")
}

// -------- 以下为 Installer 的 Node 门面方法（node.go 内实现，维持对外签名不变） --------

// isNodeTypeType 判断插件类型是否为 Node/JS。
func isNodeTypeType(t string) bool {
	lt := strings.ToLower(t)
	return strings.Contains(lt, "node") || strings.Contains(lt, "js") || strings.Contains(lt, "javascript")
}

// NodeInterp 返回某插件的 Node 解释器；未找到则 ( "", false )。
// 优先取宿主运行时声明的 node；upstream 覆盖到系统 PATH。
func (i *Installer) NodeInterp(mf lifecycle.Manifest) (string, bool) {
	if !isNodeTypeType(mf.Type) {
		return "", false
	}
	if v := os.Getenv("OCTRUN_NODE"); v != "" {
		if nodeExists(v) {
			return v, true
		}
	}
	for _, cand := range []string{"node", "node.exe"} {
		if p, err := exec.LookPath(cand); err == nil {
			return p, true
		}
	}
	return "", false
}

func nodeExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// NodePackages 返回 Electron 宿主顶层 Node 依赖（ui/package.json 的 dependencies）。
func (i *Installer) NodePackages() ([]Pkg, error) {
	uiPkg := filepath.Join(i.root, "ui", "package.json")
	b, err := os.ReadFile(uiPkg)
	if err != nil {
		return nil, err
	}
	var pj struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(b, &pj); err != nil {
		return nil, err
	}
	pkgs := make([]Pkg, 0, len(pj.Dependencies))
	for name, ver := range pj.Dependencies {
		pkgs = append(pkgs, Pkg{Name: name, Version: ver})
	}
	return pkgs, nil
}