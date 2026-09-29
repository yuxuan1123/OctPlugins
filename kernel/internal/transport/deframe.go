package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ProtocolError 一行非法协议数据的错误（§11.3：污染须启动瞬间暴露）。
type ProtocolError struct {
	Code string // 诊断码（如 E_STDOUT_CONTAMINATED）
	Unit string // 产生该行的单元 id
	Raw  string // 原始前 200 字节回显
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("%s: unit %s raw %.200s", e.Code, e.Unit, e.Raw)
}

// Deframer 按行切分 NDJSON 流（§11.3）。
// 逐行即时校验：非法行返回错误（调用方应据此终止该单元）；空行跳过；
// 超长行切断丢弃并计 warn，不终止读循环。
type Deframer struct {
	buf        []byte
	maxLineLen int
	unitID     string
	onMsg      func(json.RawMessage)
	onLongLine func(unit string, n int) // 超长行告警回调（可选）
}

// NewDeframer 构造切帧器。maxLineLen<=0 时不设单行上限。
func NewDeframer(unitID string, maxLineLen int, onMsg func(json.RawMessage)) *Deframer {
	return &Deframer{unitID: unitID, maxLineLen: maxLineLen, onMsg: onMsg}
}

// SetLongLineCallback 注册超长行告警（warn 级诊断，含单元 id 与实际长度）。
func (d *Deframer) SetLongLineCallback(fn func(unit string, n int)) { d.onLongLine = fn }

// Feed 喂入一段字节；返回 nil 表示正常消费完（含空行/超长行）。
// 返回 ProtocolError 表示出现非法 JSON 行——调用方 MUST 立即终止该单元（§11.3）。
func (d *Deframer) Feed(chunk []byte) error {
	d.buf = append(d.buf, chunk...)
	for {
		i := bytes.IndexByte(d.buf, '\n')
		if i < 0 {
			return nil
		}
		line := d.buf[:i]
		d.buf = d.buf[i+1:]
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if d.maxLineLen > 0 && len(line) > d.maxLineLen {
			// MAY：超长行切断丢弃，不终止读循环，但 MUST 记 warn 诊断。
			if d.onLongLine != nil {
				d.onLongLine(d.unitID, len(line))
			}
			continue
		}
		if !json.Valid(line) {
			return &ProtocolError{
				Code: "E_STDOUT_CONTAMINATED",
				Unit: d.unitID,
				Raw:  truncate(string(line), 200),
			}
		}
		if d.onMsg != nil {
			d.onMsg(json.RawMessage(line))
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
