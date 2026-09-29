package transport

import (
	"encoding/json"
	"errors"
	"fmt"
)

// HandshakeFrame $/handshake 首帧（§11.4）。
//
// §11.4 规定 token / protocolVersion 位于帧的 params 对象内：
//
//	{"jsonrpc":"2.0","method":"$/handshake","params":{"protocolVersion":1,"token":"<env>"}}
//
// ParseHandshake 把 params 内的字段提升到 Token / ProtocolVersion 便于校验。
type HandshakeFrame struct {
	JSONRPC         string `json:"jsonrpc"`
	Method          string `json:"method"`
	ProtocolVersion int    `json:"protocolVersion"` // 兼容顶层旧格式
	Token           string `json:"token"`           // 兼容顶层旧格式
	Params          Params `json:"params"`
}

// Params $/handshake 帧的 params 段（§11.4：token 与 protocolVersion 位于此）。
type Params struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Token           string `json:"token"`
}

// HandshakeResult 校验结果。
type HandshakeResult struct {
	OK   bool
	Code string // E_HANDSHAKE_TIMEOUT / E_HANDSHAKE_REJECTED
	Msg  string
}

var ErrHandshake = errors.New("handshake rejected")

// ParseHandshake 解析首帧；非法 JSON 返回 E_PARSE 语义错误。
// §11.4：token / protocolVersion 取自 params（旧顶层格式仍兼容）。
func ParseHandshake(line []byte) (*HandshakeFrame, error) {
	var f HandshakeFrame
	if err := json.Unmarshal(line, &f); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	// §11.4 params 优先；params 未提供时回退顶层旧格式。
	if f.Params.Token != "" {
		f.Token = f.Params.Token
	}
	if f.Params.ProtocolVersion != 0 {
		f.ProtocolVersion = f.Params.ProtocolVersion
	}
	return &f, nil
}

// VerifyHandshake 校验握手帧（§11.4 MUST）：token 与 protocolVersion 不匹配即拒绝。
// wantToken 为空串表示不做 token 校验（供单测）。
func VerifyHandshake(f *HandshakeFrame, wantToken string, wantVersion int) HandshakeResult {
	if f.Method != "$/handshake" {
		return HandshakeResult{OK: false, Code: "E_HANDSHAKE_REJECTED", Msg: "missing $/handshake method"}
	}
	if wantToken != "" && f.Token != wantToken {
		return HandshakeResult{OK: false, Code: "E_HANDSHAKE_REJECTED", Msg: "token mismatch"}
	}
	if wantVersion > 0 && f.ProtocolVersion != wantVersion {
		return HandshakeResult{OK: false, Code: "E_HANDSHAKE_REJECTED", Msg: "protocolVersion mismatch"}
	}
	return HandshakeResult{OK: true}
}
