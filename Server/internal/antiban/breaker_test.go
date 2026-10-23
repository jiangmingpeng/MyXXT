package antiban

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBreakerOpensAndCoolsDown(t *testing.T) {
	opts := testOptions()
	opts.BreakerThreshold = 2
	opts.BreakerCooldown = 40 * time.Millisecond
	engine := New(opts, newFakeStore())

	engine.Observe("13800000001", SignalRateLimited)
	if st := engine.Status("13800000001"); st.Blocked {
		t.Fatalf("未达阈值不应熔断")
	}
	engine.Observe("13800000001", SignalRateLimited)
	st := engine.Status("13800000001")
	if !st.Blocked || st.Remaining <= 0 {
		t.Fatalf("达到阈值应熔断: %+v", st)
	}

	// 冷却期内 Acquire 必须被拒绝，且不消耗并发槽位。
	_, err := engine.Acquire(context.Background(), "13800000001")
	if err == nil {
		t.Fatalf("冷却期内 Acquire 应失败")
	}
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("应返回 BlockedError，实际 %T", err)
	}

	time.Sleep(60 * time.Millisecond)
	if st := engine.Status("13800000001"); st.Blocked {
		t.Fatalf("冷却结束应恢复: %+v", st)
	}
	release, err := engine.Acquire(context.Background(), "13800000001")
	if err != nil {
		t.Fatalf("冷却结束后 Acquire 应成功: %v", err)
	}
	release()
}

func TestBreakerSuccessResetsRisk(t *testing.T) {
	opts := testOptions()
	opts.BreakerThreshold = 2
	engine := New(opts, newFakeStore())

	engine.Observe("13800000001", SignalRateLimited)
	engine.Observe("13800000001", SignalSuccess)
	engine.Observe("13800000001", SignalRateLimited)

	if st := engine.Status("13800000001"); st.Blocked {
		t.Fatalf("成功应清零风险计数: %+v", st)
	}
}

func TestBreakerIgnoresLocationErrors(t *testing.T) {
	opts := testOptions()
	opts.BreakerThreshold = 1
	engine := New(opts, newFakeStore())

	for i := 0; i < 10; i++ {
		engine.Observe("13800000001", SignalLocationRejected)
	}
	if st := engine.Status("13800000001"); st.Blocked {
		t.Fatalf("定位失败属业务错误，不应熔断: %+v", st)
	}
}

func TestAcquireIsNoopWhenDisabled(t *testing.T) {
	opts := testOptions()
	opts.Enabled = false
	engine := New(opts, nil)

	for i := 0; i < 5; i++ {
		engine.Observe("13800000001", SignalBlocked)
	}

	release, err := engine.Acquire(context.Background(), "13800000001")
	if err != nil {
		t.Fatalf("关闭后不应熔断: %v", err)
	}
	release()
}

func TestClassifyResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   Signal
	}{
		{"too many requests", 429, "", SignalRateLimited},
		{"forbidden", 403, "", SignalBlocked},
		{"server error", 502, "", SignalTransient},
		{"rate limited body", 200, `{"msg":"请求过于频繁"}`, SignalRateLimited},
		{"blocked body", 200, "账号存在风险，已被限制", SignalBlocked},
		{"location", 200, "errorLocation2_120.5", SignalLocationRejected},
		{"captcha", 200, "validate", SignalCaptcha},
		{"normal", 200, `{"status":true}`, SignalNone},
	}
	for _, tc := range cases {
		if got := ClassifyResponse(tc.status, tc.body); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestClassifySignBody(t *testing.T) {
	if got := ClassifySignBody(200, "success"); got != SignalSuccess {
		t.Fatalf("success 应识别为成功，实际 %v", got)
	}
	if got := ClassifySignBody(200, "您已签到过了"); got != SignalNone {
		t.Fatalf("已签到不应算风控信号，实际 %v", got)
	}
	if got := ClassifySignBody(200, "请求过于频繁"); got != SignalRateLimited {
		t.Fatalf("频繁应识别为限流，实际 %v", got)
	}
}

func TestNormalizeProxy(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"1.2.3.4:8080", "http://1.2.3.4:8080", false},
		{"http://user:pass@host:8080", "http://user:pass@host:8080", false},
		{"socks5://1.2.3.4:1080", "socks5://1.2.3.4:1080", false},
		{"ftp://1.2.3.4:21", "", true},
		{"http://", "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeProxy(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%q 期望报错", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q 解析失败: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("%q -> %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewTransportUsesProxy(t *testing.T) {
	tr, err := NewTransport("http://user:pass@127.0.0.1:8888", false)
	if err != nil {
		t.Fatalf("构造 transport 失败: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://mobilelearn.chaoxing.com/", nil)
	u, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("解析代理失败: %v", err)
	}
	if u == nil || u.Host != "127.0.0.1:8888" {
		t.Fatalf("代理未生效: %v", u)
	}

	direct, err := NewTransport("", false)
	if err != nil {
		t.Fatalf("构造直连 transport 失败: %v", err)
	}
	if direct.Proxy != nil {
		if u, _ := direct.Proxy(req); u != nil {
			t.Fatalf("空代理应为直连，实际 %v", u)
		}
	}
}

func TestExitIPResolverDefaultProbeURL(t *testing.T) {
	// 默认探测地址不能用 api.ipify.org：国内部分线路和代理节点会直接断连，
	// 探测失败会静默退回“空 clientip”，让 auto 模式看起来配好了其实没生效。
	r := NewExitIPResolver("", false)
	if !strings.Contains(r.probeURL, "ifconfig.me") {
		t.Fatalf("默认探测地址应为 ifconfig.me，实际 %q", r.probeURL)
	}
	if !strings.HasPrefix(r.probeURL, "https://") {
		t.Fatalf("默认探测地址应为 https，实际 %q", r.probeURL)
	}

	// 显式配置优先，且不受默认值影响。
	custom := NewExitIPResolver("https://example.com/ip", false)
	if custom.probeURL != "https://example.com/ip" {
		t.Fatalf("显式探测地址未生效: %q", custom.probeURL)
	}

	// 空代理不探测，直接返回空串（直连场景保持历史行为）。
	if got := r.Resolve(""); got != "" {
		t.Fatalf("空代理应返回空串，实际 %q", got)
	}
}
