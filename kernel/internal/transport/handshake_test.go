package transport

import "testing"

// TestParseHandshakeParamsParams 验证 §11.4 帧格式：token / protocolVersion 位于 params 内，
// 内核应正常校验通过（回归：曾因内核只读顶层导致 token mismatch）。
func TestParseHandshakeParamsParams(t *testing.T) {
	line := []byte(`{"jsonrpc":"2.0","method":"$/handshake","params":{"protocolVersion":1,"token":"TOK123"}}`)
	f, err := ParseHandshake(line)
	if err != nil {
		t.Fatalf("ParseHandshake: %v", err)
	}
	if f.Token != "TOK123" || f.ProtocolVersion != 1 {
		t.Fatalf("params 未提升：token=%q ver=%d", f.Token, f.ProtocolVersion)
	}
	res := VerifyHandshake(f, "TOK123", 1)
	if !res.OK {
		t.Fatalf("VerifyHandshake should pass: %+v", res)
	}
}

// TestParseHandshakeTopLevelBackCompat 兼容顶层旧格式。
func TestParseHandshakeTopLevelBackCompat(t *testing.T) {
	line := []byte(`{"jsonrpc":"2.0","method":"$/handshake","token":"TOK456","protocolVersion":1}`)
	f, err := ParseHandshake(line)
	if err != nil {
		t.Fatalf("ParseHandshake: %v", err)
	}
	if f.Token != "TOK456" || f.ProtocolVersion != 1 {
		t.Fatalf("顶层旧格式未兼容：token=%q ver=%d", f.Token, f.ProtocolVersion)
	}
}

// TestVerifyHandshakeTokenMismatch 校验非空 wantToken 下 token 不匹配被拒绝。
func TestVerifyHandshakeTokenMismatch(t *testing.T) {
	f, _ := ParseHandshake([]byte(`{"jsonrpc":"2.0","method":"$/handshake","params":{"protocolVersion":1,"token":"WRONG"}}`))
	res := VerifyHandshake(f, "EXPECTED", 1)
	if res.OK {
		t.Fatal("token mismatch 应被拒绝")
	}
}
