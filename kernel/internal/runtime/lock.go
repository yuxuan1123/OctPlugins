package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/octplugin/kernel/internal/lifecycle"
)

// §6 锁文件与哈希（《理想架构.md》§6.2–§6.4）。
//
//   - jsonHash：manifest.json 的 codeDeps 规范化序列化结果（§6.3），判断「意图是否变化」；
//   - lockHash：start.lock 内容哈希，作为缓存目录名（§6.3 MUST：只用 lockHash，不用 jsonHash）；
//   - 缓存目录名：<lang>-<ver>-<lockHash>（§6.4 MUST：解释器版本须进入目录名）。

// JSONHash 计算 codeDeps 的规范化序列化哈希（只覆盖 codeDeps 子树，§6.3）。
func JSONHash(codeDeps lifecycle.CodeDepsDecl) string {
	b, err := json.Marshal(codeDeps) // map/slice 经 encoding/json 按键排序 → 规范化输出
	if err != nil {
		return ""
	}
	return sha256Hex(b)
}

// LockHash 计算锁文件内容哈希（lockHash，§6.2/§6.3）。文件缺失返回空串。
func (i *Installer) LockHash(lockPath string) string {
	b, err := os.ReadFile(lockPath)
	if err != nil {
		return ""
	}
	return sha256Hex(b)[:16]
}

// lockfilePath 返回某插件声明的 lockfile 路径（默认 start.lock）。
func (i *Installer) lockfilePath(mf lifecycle.Manifest) string {
	name := mf.CodeDeps.Lockfile
	if strings.TrimSpace(name) == "" {
		name = "start.lock"
	}
	return filepath.Join(mf.Dir, name)
}

// GenerateLock 在 runtime.go 门面按语言委托给对应 Provider（§6.5）。

// EnsureDeps 对齐 §6.4 的 EnsureDeps：
//  1. jsonHash 变化或 lock 缺失 → 重新生成 lock；
//  2. lockHash 作为缓存目录名；缓存目录不存在 → 后台安装（调用方负责触发）。
//
// 返回缓存目录路径（相对项目根，如 runtime/python-3.12-<lockHash>）。
func (i *Installer) EnsureDeps(mf lifecycle.Manifest, jsonHash string) (string, error) {
	workDir := filepath.Join(i.root, "runtime")
	lockPath := i.lockfilePath(mf)

	// 1) 意向变化 → 重新解析
	if jsonHash != "" {
		if _, err := os.Stat(lockPath); err != nil || i.jsonChanged(mf, jsonHash) {
			if err := i.GenerateLock(mf); err != nil {
				return "", err
			}
		}
	} else if _, err := os.Stat(lockPath); err != nil {
		if err := i.GenerateLock(mf); err != nil {
			return "", err
		}
	}

	// 2) 缓存目录名
	lockHash := i.LockHash(lockPath)
	if lockHash == "" {
		return "", nil
	}
	ver := i.interpVersion(mf)
	cacheDir := filepath.Join(workDir, fmt.Sprintf("%s-%s-%s", i.langTag(mf), ver, lockHash))
	return cacheDir, nil
}

// jsonChanged 依据 codeDeps 的 jsonHash 判断意向是否变化（§6.4 表）。
func (i *Installer) jsonChanged(mf lifecycle.Manifest, current string) bool {
	return JSONHash(mf.CodeDeps) != current
}

// DepsPlan 一次性求 §6 的记录四元组：jsonHash / lockHash / cacheDir，供装配层
// spawn 前判定并写入 registry（depsState/cacheDir；§8）。错误来自重解析锁（uv pip compile）。
func (i *Installer) DepsPlan(mf lifecycle.Manifest) (jsonHash, lockHash, cacheDir string, err error) {
	jsonHash = JSONHash(mf.CodeDeps)
	if cacheDir, err = i.EnsureDeps(mf, jsonHash); err != nil {
		return
	}
	lockHash = i.LockHash(i.lockfilePath(mf))
	return
}

func (i *Installer) langTag(mf lifecycle.Manifest) string {
	if isNodeTypeType(mf.Type) {
		return "node"
	}
	if isPythonType(mf.Type) {
		return "python"
	}
	return strings.ToLower(strings.TrimSpace(mf.Type))
}

// interpVersion 求解释器大版本（用作目录名一部分；未解析回退 "any"）。
func (i *Installer) interpVersion(mf lifecycle.Manifest) string {
	v := mf.CodeDeps.Runtime.Version
	if v == "" {
		if mf.Py3v != "" {
			v = mf.Py3v
		}
	}
	v = strings.TrimLeft(strings.TrimSpace(v), ">=^~")
	if v == "" {
		return "any"
	}
	// 取首段数字（如 3.12.x → 3.12）
	parts := strings.SplitN(v, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
