package xxt

import "testing"

func TestIsQRCodeExpired(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"签到失败，请重新扫描。", true},
		{`{"msg":"签到失败，请重新扫描。"}`, true},
		{`{"error":"请重新扫描"}`, true},
		{"二维码已过期，请重新扫描", true},
		{"success", false},
		{"该同学已在学习通签到", false},
		{"活动已结束", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsQRCodeExpired(c.body); got != c.want {
			t.Errorf("IsQRCodeExpired(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}
