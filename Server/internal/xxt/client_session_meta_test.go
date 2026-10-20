package xxt

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"
)

// 登录后 fid 优先从 _fid cookie 读取；cookie 缺失时退回 base 页面解析。
func TestFidFromJar(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	jar.SetCookies(&url.URL{Scheme: "https", Host: "i.chaoxing.com"}, []*http.Cookie{
		{Name: "_fid", Value: "12345"},
	})
	if got := fidFromJar(jar); got != 12345 {
		t.Fatalf("fidFromJar = %d, want 12345", got)
	}
}

// 登录表单里的 fid=-1 不能被误当成学校 fid。
func TestFidFromJarIgnoresNegative(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	jar.SetCookies(&url.URL{Scheme: "https", Host: "i.chaoxing.com"}, []*http.Cookie{
		{Name: "_fid", Value: "-1"},
	})
	if got := fidFromJar(jar); got != 0 {
		t.Fatalf("fidFromJar = %d, want 0", got)
	}
}

func TestFidFromHTML(t *testing.T) {
	cases := []struct {
		html string
		want int64
	}{
		{`<script>var _fid = "12345";</script>`, 12345},
		{`{"fid":67890}`, 67890},
		{`<a href="/x?fid=24680&t=1">`, 24680},
		{`<script>var _fid = "-1";</script>`, 0},
		{`nothing here`, 0},
	}
	for _, c := range cases {
		if got := fidFromHTML(c.html); got != c.want {
			t.Errorf("fidFromHTML(%q) = %d, want %d", c.html, got, c.want)
		}
	}
}

// deviceCode 必须是 32 位小写 hex，且每次生成都不同。
func TestNewDeviceCode(t *testing.T) {
	a := newDeviceCode()
	b := newDeviceCode()
	if len(a) != 32 {
		t.Fatalf("deviceCode length = %d, want 32", len(a))
	}
	for _, r := range a {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			t.Fatalf("deviceCode %q is not lowercase hex", a)
		}
	}
	if a == b {
		t.Fatalf("deviceCode should be random, got %q twice", a)
	}
}
