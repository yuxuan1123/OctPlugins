// Command protocolgen 从 pkg/protocol 的 ErrorTable（§19 唯一真源）派生四份语言常量，
// 供 SDK（Node/Python）与宿主 UI（TS）复用，杜绝各自硬编码错误码字符串。
//
// 用法：在 kernel/pkg/protocol 目录下执行 `go generate ./...`。
// 产物（均有「generated do not edit」标注）：
//   - kernel/pkg/protocol/errors.json       （方言中立）
//   - sdk/node/src/protocol_errors.js
//   - sdk/python/oct_sdk/protocol_errors.py
//   - ui/src/protocol-errors.ts
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/octplugin/kernel/pkg/protocol"
)

// repoRoot 从本包工作目录（go:generate 在 package dir 运行）回退到仓库根。
func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	// kernel/pkg/protocol → 上溯 3 级 = 仓库根
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}

func sorted(items []protocol.ErrorTableItem) []protocol.ErrorTableItem {
	c := append([]protocol.ErrorTableItem(nil), items...)
	sort.Slice(c, func(i, j int) bool { return c[i].Diag < c[j].Diag })
	return c
}

func main() {
	root := repoRoot()
	items := sorted(protocol.ErrorTable)

	if err := os.WriteFile(filepath.Join(root, "kernel", "pkg", "protocol", "errors.json"),
		jsonBytes(items), 0o644); err != nil {
		panic(fmt.Errorf("write errors.json: %w", err))
	}
	if err := writeJS(filepath.Join(root, "sdk", "node", "src", "protocol_errors.js"), items); err != nil {
		panic(err)
	}
	if err := writePy(filepath.Join(root, "sdk", "python", "oct_sdk", "protocol_errors.py"), items); err != nil {
		panic(err)
	}
	if err := writeTS(filepath.Join(root, "ui", "src", "protocol-errors.ts"), items); err != nil {
		panic(err)
	}
	fmt.Printf("protocolgen: emitted %d error code entries\n", len(items))
}

func jsonBytes(items []protocol.ErrorTableItem) []byte {
	type row struct {
		Code int    `json:"code"`
		Diag string `json:"diag"`
		Msg  string `json:"msg"`
	}
	rows := make([]row, 0, len(items))
	for _, it := range items {
		rows = append(rows, row{it.Code, it.Diag, it.Msg})
	}
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func writeJS(path string, items []protocol.ErrorTableItem) error {
	var sb strings.Builder
	sb.WriteString("// GENERATED from pkg/protocol §19 — do not edit. Regenerate: `go generate ./...` in kernel/pkg/protocol.\n")
	sb.WriteString("'use strict';\n")
	sb.WriteString("const CODES = {\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "  %s: %d,\n", it.Diag, it.Code)
	}
	sb.WriteString("};\nconst MESSAGES = {\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "  %s: %s,\n", it.Diag, jsonStr(it.Msg))
	}
	sb.WriteString("};\nconst DIAG_BY_CODE = {\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "  %q: %q,\n", strconv.Itoa(it.Code), it.Diag)
	}
	sb.WriteString("};\nmodule.exports = { CODES, MESSAGES, DIAG_BY_CODE };\n")
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func writePy(path string, items []protocol.ErrorTableItem) error {
	var sb strings.Builder
	sb.WriteString("# GENERATED from pkg/protocol §19 -- do not edit. Regenerate: `go generate ./...` in kernel/pkg/protocol.\n")
	sb.WriteString("CODES = {\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "    %q: %d,\n", it.Diag, it.Code)
	}
	sb.WriteString("}\nMESSAGES = {\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "    %q: %s,\n", it.Diag, pyStr(it.Msg))
	}
	sb.WriteString("}\nDIAG_BY_CODE = {\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "    %d: %q,\n", it.Code, it.Diag)
	}
	sb.WriteString("}\n")
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func writeTS(path string, items []protocol.ErrorTableItem) error {
	var sb strings.Builder
	sb.WriteString("// GENERATED from pkg/protocol §19 — do not edit. Regenerate: `go generate ./...` in kernel/pkg/protocol.\n")
	sb.WriteString("export const ECodes = {\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "  %s: %d,\n", it.Diag, it.Code)
	}
	sb.WriteString("} as const;\n\nexport const EMessages: Record<string, string> = {\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "  %s: %s,\n", it.Diag, jsonStr(it.Msg))
	}
	sb.WriteString("};\n\n// EDiagByCode 运行时派生（避免负数数字键的字面量歧义）。\n")
	sb.WriteString("export const EDiagByCode: Record<number, string> = Object.fromEntries(\n")
	sb.WriteString("  (Object.entries(ECodes) as Array<[string, number]>).map(([k, v]) => [v, k])\n")
	sb.WriteString(");\n")
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func pyStr(s string) string {
	// Python 字符串转义（ASCII 引号+转义即可，无需 \u 前缀差异）。
	return strconv.Quote(s)
}