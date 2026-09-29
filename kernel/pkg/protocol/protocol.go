package protocol

import "encoding/json"

//go:generate go run github.com/octplugin/kernel/cmd/protocolgen

// JSON-RPC 2.0 扩展信封（M1-WS消息Schema）。v=1。

const ProtocolVersion = 1

type Request struct {
	V       int             `json:"v"`
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	V       int       `json:"v"`
	JSONRPC string    `json:"jsonrpc"`
	ID      int64     `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type Notification struct {
	V       int    `json:"v"`
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// 业务错误码（对应 M1-WS消息Schema §9）
const (
	ErrParse          = -32700
	ErrMethodNotFound = -32601
	ErrPluginDown     = -32000
	ErrTimeout        = -32001
	ErrPluginCrashed  = -32002
	ErrPluginMissing  = -32003
	ErrPluginState    = -32004
	ErrPermDenied     = -32005
	ErrHotkeyConflict = -32006
	ErrDepsMissing    = -32007
	ErrDepsInstall    = -32008
	ErrStreamClosed   = -32009
	ErrIO             = -32010
	// §19：-32011~-32018 与 codes.go 的 E_* 诊断码一一对应
	ErrHandshakeTimeout  = -32011
	ErrHandshakeRejected = -32012
	ErrStdoutContam      = -32013
	ErrDepsHashMismatch  = -32014
	ErrBusyTimeout       = -32015
	ErrToolUnavailable   = -32016
	ErrModelLoadTimeout  = -32017
	ErrVRAMInsufficient  = -32018
)

var ErrText = map[int]string{
	ErrParse:          "parse error",
	ErrMethodNotFound: "method not found",
	ErrPluginDown:     "plugin process not running",
	ErrTimeout:        "call timeout",
	ErrPluginCrashed:  "plugin crashed",
	ErrPluginMissing:  "plugin not found",
	ErrPluginState:    "plugin invalid state",
	ErrPermDenied:     "permission denied",
	ErrHotkeyConflict: "hotkey conflict",
	ErrDepsMissing:    "dependency not installed",
	ErrDepsInstall:    "dependency install failed",
	ErrStreamClosed:   "stream closed or missing",
	ErrIO:             "io error",

	ErrHandshakeTimeout:  "handshake timed out",
	ErrHandshakeRejected: "handshake rejected (bad token/version)",
	ErrStdoutContam:      "stdout contaminated with non-JSON data",
	ErrDepsHashMismatch:  "dependency lock hash mismatch",
	ErrBusyTimeout:       "busy deadline exceeded and ping unresponsive",
	ErrToolUnavailable:   "tool unavailable (locate failed)",
	ErrModelLoadTimeout:  "model load exceeded timeout",
	ErrVRAMInsufficient:  "insufficient VRAM with no usable fallback",
}

func NewResult(id int64, res any) Response {
	return Response{V: ProtocolVersion, JSONRPC: "2.0", ID: id, Result: res}
}

// ErrorTable 是 §19 错误码表的唯一真源。
// 数字码（code，走线上协议）、诊断码（diag，走 error.data.diag，供日志/UI 文案映射）与
// 提示文本（msg）三列并存。SDK（Node/Python）与 UI **必须**经 protocolgen 从此派生
// 常量（见 cmd/protocolgen + //go:generate protocol.go），禁止各自硬编码。
type ErrorTableItem struct {
	Code int    `json:"code"`
	Diag string `json:"diag"`
	Msg  string `json:"msg"`
}

var ErrorTable = []ErrorTableItem{
	{ErrParse, DiagParse, ErrText[ErrParse]},
	{ErrMethodNotFound, DiagMethodNotFound, ErrText[ErrMethodNotFound]},
	{ErrPluginDown, DiagPluginDown, ErrText[ErrPluginDown]},
	{ErrTimeout, DiagTimeout, ErrText[ErrTimeout]},
	{ErrPluginCrashed, DiagPluginCrashed, ErrText[ErrPluginCrashed]},
	{ErrPluginMissing, DiagPluginMissing, ErrText[ErrPluginMissing]},
	{ErrPluginState, DiagPluginState, ErrText[ErrPluginState]},
	{ErrPermDenied, DiagPermissionDenied, ErrText[ErrPermDenied]},
	{ErrHotkeyConflict, DiagHotkeyConflict, ErrText[ErrHotkeyConflict]},
	{ErrDepsMissing, DiagDepsMissing, ErrText[ErrDepsMissing]},
	{ErrDepsInstall, DiagDepsInstallFail, ErrText[ErrDepsInstall]},
	{ErrStreamClosed, DiagStreamClosed, ErrText[ErrStreamClosed]},
	{ErrIO, DiagIO, ErrText[ErrIO]},
	{ErrHandshakeTimeout, DiagHandshakeTimeout, ErrText[ErrHandshakeTimeout]},
	{ErrHandshakeRejected, DiagHandshakeReject, ErrText[ErrHandshakeRejected]},
	{ErrStdoutContam, DiagStdoutContam, ErrText[ErrStdoutContam]},
	{ErrDepsHashMismatch, DiagDepsHashMismatch, ErrText[ErrDepsHashMismatch]},
	{ErrBusyTimeout, DiagBusyTimeout, ErrText[ErrBusyTimeout]},
	{ErrToolUnavailable, DiagToolUnavailable, ErrText[ErrToolUnavailable]},
	{ErrModelLoadTimeout, DiagModelLoadTimeout, ErrText[ErrModelLoadTimeout]},
	{ErrVRAMInsufficient, DiagVRAMInsufficient, ErrText[ErrVRAMInsufficient]},
}

// DiagOf 返回数字码对应的诊断码字符串（找不到返回空串）。
func DiagOf(code int) string {
	for _, it := range ErrorTable {
		if it.Code == code {
			return it.Diag
		}
	}
	return ""
}

func NewError(id int64, code int, data any) Response {
	// §19 MUST：面向用户的错误提示必须附带诊断码（error.data.diag）。
	d := map[string]any{}
	if m, ok := data.(map[string]any); ok {
		for k, v := range m {
			d[k] = v
		}
	}
	if _, has := d["diag"]; !has {
		d["diag"] = DiagOf(code)
	}
	return Response{V: ProtocolVersion, JSONRPC: "2.0", ID: id,
		Error: &RPCError{Code: code, Message: ErrText[code], Data: d}}
}

func (e Response) Marshal() []byte {
	b, _ := json.Marshal(e)
	return b
}
