//go:build windows

package resources

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// registryToolPaths 按系统注册表登记返回工具的可执行路径（env 定位兜底之一）。
// 当前覆盖 LibreOffice（HKLM\SOFTWARE\LibreOffice\LibreOffice\<ver>\Path /
// UNO\InstallPath），其它工具可随登记扩展。未命中返回 nil。
func registryToolPaths(id string) []string {
	switch id {
	case "soffice", "libreoffice", "soffice.bin", "soffice.exe":
		return libreOfficePaths()
	}
	return nil
}

func libreOfficePaths() []string {
	var out []string
	roots := []string{
		`SOFTWARE\LibreOffice\LibreOffice`,             // 64 位安装
		`SOFTWARE\WOW6432Node\LibreOffice\LibreOffice`, // 32 位安装
	}
	for _, root := range roots {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, root, registry.READ)
		if err != nil {
			continue
		}
		// 遍历版本子键（25.8 / 24.2 …）：任一含 Path/InstallPath 值即命中。
		vers, err := k.ReadSubKeyNames(0)
		_ = k.Close()
		if err != nil {
			continue
		}
		for _, ver := range vers {
			vk, err := registry.OpenKey(registry.LOCAL_MACHINE, root+`\`+ver, registry.READ)
			if err != nil {
				continue
			}
			for _, name := range []string{"Path", "InstallPath"} {
				val, _, err := vk.GetStringValue(name)
				if err != nil || val == "" {
					continue
				}
				// val 可能是安装目录（…\LibreOffice）也可能是 exe 路径（…\program\soffice.exe），
				// 全形态追加，由 locateCommon 的 os.Stat 过滤。
				out = append(out, val,
					fmt.Sprintf(`%s\soffice.exe`, val),
					fmt.Sprintf(`%s\program\soffice.exe`, val),
					fmt.Sprintf(`%s\program\soffice.com`, val))
			}
			_ = vk.Close()
		}
	}
	// 兜底：UNO\InstallPath（较老的安装布局）。
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\LibreOffice\UNO`, registry.READ); err == nil {
		if val, _, err := k.GetStringValue("InstallPath"); err == nil && val != "" {
			out = append(out, val, fmt.Sprintf(`%s\soffice.exe`, val))
		}
		_ = k.Close()
	}
	return out
}
