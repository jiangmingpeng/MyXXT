package xxt

import (
	"net/url"
	"strings"
	"testing"
)

// 刷新二维码的 rcode 只能被 URL 编码一次。
// 旧实现先在 Set 前 QueryEscape，Encode() 又转义一次，学习通拿到的是
// SIGNIN%253Aaid...，导致定时刷新二维码始终预签到失败。
func TestExtractAnalysisCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{"plain plus", `<script>var code='+abc123';</script>`, "abc123", true},
		{"single backslash", `<script>var code='\+abc123';</script>`, "abc123", true},
		{"double backslash", `<script>var code='\\+abc123';</script>`, "abc123", true},
		{"quoted concat", `<script>var code='\+'abc123';</script>`, "abc123", true},
		{"missing", "no code here", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractAnalysisCode([]byte(c.body))
			if ok != c.ok || got != c.want {
				t.Fatalf("extractAnalysisCode(%q) = (%q, %v), want (%q, %v)", c.body, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestBuildPreSignRCodeEncodedOnce(t *testing.T) {
	const (
		activeID = int64(6000177117848)
		code     = "44WWXX"
		enc      = "76D86FA36D5376C34CDEFB4A79E50EDD"
	)

	vals := url.Values{}
	vals.Set("rcode", buildPreSignRCode(activeID, code, enc))
	got := vals.Encode()

	if strings.Contains(got, "%253A") || strings.Contains(got, "%2526") {
		t.Fatalf("rcode was double-encoded: %s", got)
	}
	want := "rcode=SIGNIN%3Aaid%3D6000177117848%26source%3D15%26Code%3D44WWXX%26enc%3D76D86FA36D5376C34CDEFB4A79E50EDD"
	if got != want {
		t.Fatalf("unexpected rcode encoding:\n got: %s\nwant: %s", got, want)
	}

	// 解码一次后应还原为学习通期望的明文 rcode。
	decoded, err := url.QueryUnescape(got[len("rcode="):])
	if err != nil {
		t.Fatalf("decode rcode: %v", err)
	}
	if wantRCode := "SIGNIN:aid=6000177117848&source=15&Code=44WWXX&enc=76D86FA36D5376C34CDEFB4A79E50EDD"; decoded != wantRCode {
		t.Fatalf("decoded rcode = %q, want %q", decoded, wantRCode)
	}
}

func TestBuildAnalysisQuery(t *testing.T) {
	t.Run("static qr falls back to RANDOM", func(t *testing.T) {
		vals := buildAnalysisQuery(123, "", "")
		if got := vals.Get("DB_STRATEGY"); got != "RANDOM" {
			t.Fatalf("DB_STRATEGY = %q, want RANDOM", got)
		}
		if got := vals.Get("STRATEGY_PARA"); got != "" {
			t.Fatalf("STRATEGY_PARA = %q, want empty", got)
		}
		if got := vals.Get("aid"); got != "123" {
			t.Fatalf("aid = %q, want 123", got)
		}
	})

	t.Run("refresh qr keeps PRIMARY_KEY strategy", func(t *testing.T) {
		vals := buildAnalysisQuery(6000177117848, "PRIMARY_KEY", "id")
		if got := vals.Get("DB_STRATEGY"); got != "PRIMARY_KEY" {
			t.Fatalf("DB_STRATEGY = %q, want PRIMARY_KEY", got)
		}
		if got := vals.Get("STRATEGY_PARA"); got != "id" {
			t.Fatalf("STRATEGY_PARA = %q, want id", got)
		}
		if got := vals.Get("id"); got != "6000177117848" {
			t.Fatalf("id = %q, want activity id", got)
		}
		if got := vals.Get("aid"); got != "6000177117848" {
			t.Fatalf("aid = %q, want activity id", got)
		}
	})
}
