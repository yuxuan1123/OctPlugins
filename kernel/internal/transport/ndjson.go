// Package transport 提供插件信道的帧传输原语（§11）。
//
// 对齐《理想架构.md》§11.2–§11.4：
//   - NDJSON：每行一条 JSON，以 \n 结尾，禁止 pretty-print（§11.3）；
//   - Deframer 按行切分并即时校验：非法行立即报错（污染即暴露），不静默丢弃；
//   - Handshake 校验握手帧的 token 与 protocolVersion（§11.4）。
package transport

import "encoding/json"

// NDJSONLine 把 v 序列化为一条 NDJSON 行（标准序列化器对换行自动转义）。
func NDJSONLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ValidLine 校验一行是否为合法 JSON（§11.3 的即时校验）。
func ValidLine(line []byte) bool {
	return json.Valid(line)
}
