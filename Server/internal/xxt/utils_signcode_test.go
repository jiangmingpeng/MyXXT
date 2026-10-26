package xxt

import (
	"encoding/json"
	"testing"
)

func TestCodeFromAny(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"string", "1730000000123", "1730000000123"},
		{"json number", float64(1730000000123), "1730000000123"},
		{"small number", float64(42), "42"},
		{"int", 7, "7"},
		{"non-integer", 1.5, "1.5"},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		if got := codeFromAny(c.in); got != c.want {
			t.Errorf("%s: codeFromAny(%v) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// 主动过期探测直接拿 sign_code 和二维码的 c 参数比字符串，
// 这里锁死两种下发形态（JSON number / JSON string）都要还原成纯数字文本，
// 否则科学计数法会让每一张码都被误判成“已失效”。
func TestSignDetailFromPayloadExpiryFields(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
		wantOver bool
	}{
		{"number code, running", `{"signCode":1730000000123,"isOver":0}`, "1730000000123", false},
		{"string code, over", `{"signCode":"1730000000123","isOver":1}`, "1730000000123", true},
		{"nested fallback", `{"data":{"signCode":42,"isOver":0}}`, "42", false},
		{"missing fields", `{}`, "", false},
	}
	for _, c := range cases {
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(c.body), &payload); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		detail := signDetailFromPayload(payload)
		if detail.SignCode != c.wantCode {
			t.Errorf("%s: SignCode = %q, want %q", c.name, detail.SignCode, c.wantCode)
		}
		if detail.IfOver != c.wantOver {
			t.Errorf("%s: IfOver = %v, want %v", c.name, detail.IfOver, c.wantOver)
		}
	}
}
