package xxt

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingTransport 记录所有请求，避免单测访问真实学习通。
type recordingTransport struct {
	mu    sync.Mutex
	hosts []string
	paths []string
	query []url.Values
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.hosts = append(r.hosts, req.URL.Host)
	r.paths = append(r.paths, req.URL.Path)
	r.query = append(r.query, req.URL.Query())
	r.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("success")),
		Request:    req,
	}, nil
}

// 预签到与验证码并行后，SignService 会把预抓的 validate 放进 special["validate"]。
// 二维码签到必须直接复用该 validate，不能再去访问验证码服务（否则等于白抓，且拖慢刷新窗口）。
func TestSignQRCodeReusesPrefetchedValidate(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	rt := &recordingTransport{}
	c := &Client{
		mobileUA: "xbt-test",
		http:     &http.Client{Transport: rt},
		sessions: map[string]*Session{},
	}
	c.sessions["13800000000"] = &Session{
		Mobile:      "13800000000",
		Password:    "pw",
		Name:        "测试",
		Jar:         jar,
		LastLoginAt: time.Now(),
	}

	fixed := FixedParams{ActiveID: 1, UID: 2, CourseID: 3, ClassID: 4}
	got, err := c.Sign("13800000000", "pw", fixed, SignQRCode, map[string]interface{}{
		"enc":      "ENC123",
		"validate": "VALIDATE456",
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if got != "success" {
		t.Fatalf("result = %q, want success", got)
	}

	if len(rt.paths) != 1 {
		t.Fatalf("requests = %v %v, want exactly 1", rt.hosts, rt.paths)
	}
	if rt.hosts[0] != "mobilelearn.chaoxing.com" || !strings.HasSuffix(rt.paths[0], "/pptSign/stuSignajax") {
		t.Fatalf("unexpected request: %s%s", rt.hosts[0], rt.paths[0])
	}
	if v := rt.query[0].Get("validate"); v != "VALIDATE456" {
		t.Fatalf("validate = %q, want VALIDATE456", v)
	}
	if enc := rt.query[0].Get("enc"); enc != "ENC123" {
		t.Fatalf("enc = %q, want ENC123", enc)
	}
}

// 二维码签到必须带上学校 fid、课程 id、设备标识与反作弊参数，
// 并额外发送 locationResult（带 mockData 字符串的第二个定位参数）。
func TestSignQRCodeSendsFidAndAntiCheatParams(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	rt := &recordingTransport{}
	c := &Client{
		mobileUA: "xbt-test",
		http:     &http.Client{Transport: rt},
		sessions: map[string]*Session{},
	}
	c.sessions["13800000000"] = &Session{
		Mobile:      "13800000000",
		Password:    "pw",
		UID:         2,
		Name:        "测试",
		Fid:         12345,
		DeviceCode:  "devcode0123456789",
		Jar:         jar,
		LastLoginAt: time.Now(),
	}

	fixed := FixedParams{ActiveID: 1, UID: 2, CourseID: 3, ClassID: 4}
	got, err := c.Sign("13800000000", "pw", fixed, SignQRCode, map[string]interface{}{
		"enc":         "ENC123",
		"validate":    "VALIDATE456",
		"latitude":    30.5,
		"longitude":   120.5,
		"description": "图书馆",
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if got != "success" {
		t.Fatalf("result = %q, want success", got)
	}
	if len(rt.query) != 1 {
		t.Fatalf("requests = %v, want exactly 1", rt.paths)
	}

	q := rt.query[0]
	for key, want := range map[string]string{
		"fid":           "12345",
		"courseId":      "3",
		"deviceCode":    "devcode0123456789",
		"vpProbability": "-1",
		"latitude":      "-1",
		"longitude":     "-1",
	} {
		if v := q.Get(key); v != want {
			t.Errorf("%s = %q, want %q", key, v, want)
		}
	}
	// vpStrategy 需要显式携带空值（与 ChaoxingSignFaker 一致）。
	if _, exists := q["vpStrategy"]; !exists {
		t.Errorf("vpStrategy should be present (empty), query=%v", q)
	}
	// 二维码签到不应携带空 useragent。
	if _, exists := q["useragent"]; exists {
		t.Errorf("useragent should be absent, got %q", q.Get("useragent"))
	}

	var location map[string]interface{}
	if err := json.Unmarshal([]byte(q.Get("location")), &location); err != nil {
		t.Fatalf("unmarshal location: %v", err)
	}
	if location["result"] != float64(1) {
		t.Errorf("location.result = %#v, want number 1", location["result"])
	}
	if _, exists := location["mockData"]; exists {
		t.Errorf("location should not carry mockData, got %v", location["mockData"])
	}

	var locationResult map[string]interface{}
	if err := json.Unmarshal([]byte(q.Get("locationResult")), &locationResult); err != nil {
		t.Fatalf("unmarshal locationResult: %v", err)
	}
	if locationResult["mockData"] != `{"strategy":0,"probability":-1}` {
		t.Errorf("locationResult.mockData = %#v, want anti-mock JSON string", locationResult["mockData"])
	}
}

// 没有预抓验证码时才允许 Sign 自己同步抓取（这里只验证不会把空 validate 当成已复用）。
func TestSignQRCodeWithoutPrefetchedValidateFallsBack(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	rt := &recordingTransport{}
	c := &Client{
		mobileUA: "xbt-test",
		http:     &http.Client{Transport: rt},
		sessions: map[string]*Session{},
	}
	c.sessions["13800000000"] = &Session{
		Mobile:      "13800000000",
		Password:    "pw",
		Name:        "测试",
		Jar:         jar,
		LastLoginAt: time.Now(),
	}

	fixed := FixedParams{ActiveID: 1, UID: 2, CourseID: 3, ClassID: 4}
	_, _ = c.Sign("13800000000", "pw", fixed, SignQRCode, map[string]interface{}{
		"enc": "ENC123",
	})

	// validate 为空走兜底抓取：会先访问验证码服务，再提交签到。
	var captchaHits int
	for _, host := range rt.hosts {
		if host == "captcha.chaoxing.com" {
			captchaHits++
		}
	}
	if captchaHits == 0 {
		t.Fatalf("expected captcha fallback request, hosts=%v", rt.hosts)
	}
}
