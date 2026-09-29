package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/octplugin/kernel/internal/app"
)

func main() {
	// 项目根 = 内核二进制所在目录的上一级（.../kernel/ → 项目根）。
	// 依赖隔离 deps/<id>、宿主 ui、store 等均以项目根为基准（见 environment.md）。
	bin := os.Args[0]
	if exe, e2 := os.Executable(); e2 == nil && exe != "" {
		bin = exe
	}
	root, err := filepath.Abs(filepath.Dir(filepath.Dir(bin)))
	if err != nil {
		root, _ = os.Getwd()
	}
	if err := app.Run(root); err != nil {
		log.Fatalf("[kernel] %v", err)
	}
}
