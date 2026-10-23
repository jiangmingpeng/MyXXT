package xxt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newFlightClient() *Client {
	return &Client{}
}

// 单飞：同一活动 + 账号的并发抓取只允许真正执行一次。
// 这是"预取失败后 Sign 又解一遍"导致单次签到耗时翻倍的那条路径的回归测试。
func TestCaptchaFlightSingleFlight(t *testing.T) {
	c := newFlightClient()
	key := captchaFlightKey("13800000000", FixedParams{ActiveID: 1, UID: 2})

	var calls int32
	release := make(chan struct{})

	fl1, created := c.captchaFlightStart(key)
	if !created {
		t.Fatal("first call should create the flight")
	}
	go func() {
		<-release
		c.captchaFlightFinish(key, fl1, "VALIDATE", nil)
	}()

	fl2, created2 := c.captchaFlightStart(key)
	if created2 {
		t.Fatal("second call must reuse the in-flight task")
	}
	if fl2 != fl1 {
		t.Fatal("second call returned a different flight")
	}

	// 第一个调用者执行求解。
	atomic.AddInt32(&calls, 1)
	close(release)

	<-fl1.done
	<-fl2.done
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("solve executed %d times, want 1", got)
	}
	if fl1.val != "VALIDATE" || fl2.val != "VALIDATE" {
		t.Fatalf("values = %q/%q, want VALIDATE", fl1.val, fl2.val)
	}
}

// 非阻塞取值：在途时必须立刻返回 false，完成后只能被消费一次。
func TestCaptchaFlightTakeNonBlocking(t *testing.T) {
	c := newFlightClient()
	fixed := FixedParams{ActiveID: 7, UID: 8}
	mobile := "13900000000"

	fl, created := c.captchaFlightStart(captchaFlightKey(mobile, fixed))
	if !created {
		t.Fatal("flight should be created")
	}

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		close(done)
		<-finished
		c.captchaFlightFinish(captchaFlightKey(mobile, fixed), fl, "VAL2", nil)
	}()
	<-done

	// 在途：不允许阻塞。
	start := time.Now()
	if _, ok := c.TakePrefetchedCaptcha(mobile, fixed); ok {
		t.Fatal("take should return false while in flight")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("take blocked for %s, want non-blocking", elapsed)
	}

	close(finished)
	<-fl.done

	got, ok := c.TakePrefetchedCaptcha(mobile, fixed)
	if !ok || got != "VAL2" {
		t.Fatalf("take after finish = (%q, %v), want (VAL2, true)", got, ok)
	}
	// validate 只能用一次：再次取值必须拿不到，强制下一次签到重新识别。
	if _, ok := c.TakePrefetchedCaptcha(mobile, fixed); ok {
		t.Fatal("take must consume the value")
	}
}

// 预取失败时 Sign 的兜底必须直接复用失败结果，而不是再解一遍验证码。
func TestCaptchaFlightAwaitReusesFailure(t *testing.T) {
	c := newFlightClient()
	fixed := FixedParams{ActiveID: 9, UID: 10}
	mobile := "13700000000"
	key := captchaFlightKey(mobile, fixed)

	fl, created := c.captchaFlightStart(key)
	if !created {
		t.Fatal("flight should be created")
	}
	wantErr := errors.New("proxy timeout")
	c.captchaFlightFinish(key, fl, "", wantErr)

	// 非阻塞取值不消费失败结果，留给 Sign 的兜底判断。
	if v, ok := c.TakePrefetchedCaptcha(mobile, fixed); ok || v != "" {
		t.Fatalf("take should not report success for a failed flight, got (%q, %v)", v, ok)
	}
	// 兜底直接复用这次失败：不再访问验证码服务（c.http 为 nil，走网络会 panic）。
	v, err := c.awaitCaptchaPrefetch(mobile, fixed)
	if !errors.Is(err, wantErr) {
		t.Fatalf("await err = %v, want %v", err, wantErr)
	}
	if v != "" {
		t.Fatalf("await val = %q, want empty", v)
	}
}

// 预取成功时 Sign 的兜底必须复用同一个 validate（同样不会走网络）。
func TestCaptchaFlightAwaitReusesSuccess(t *testing.T) {
	c := newFlightClient()
	fixed := FixedParams{ActiveID: 11, UID: 12}
	mobile := "13300000000"
	key := captchaFlightKey(mobile, fixed)

	fl, created := c.captchaFlightStart(key)
	if !created {
		t.Fatal("flight should be created")
	}
	c.captchaFlightFinish(key, fl, " VALIDATE-X ", nil)

	v, err := c.awaitCaptchaPrefetch(mobile, fixed)
	if err != nil {
		t.Fatalf("await err = %v", err)
	}
	if v != "VALIDATE-X" {
		t.Fatalf("await val = %q, want VALIDATE-X", v)
	}
	// 已被消费：再来一次说明这张 validate 已经用掉了。
	if _, ok := c.TakePrefetchedCaptcha(mobile, fixed); ok {
		t.Fatal("validate should be consumed by await")
	}
}

// 不同账号 / 不同活动不能共享 validate。
func TestCaptchaFlightKeyScopedByAccountAndActivity(t *testing.T) {
	mobile := "13600000000"
	fixedA := FixedParams{ActiveID: 1, UID: 2}
	fixedB := FixedParams{ActiveID: 1, UID: 3}
	fixedC := FixedParams{ActiveID: 5, UID: 2}

	keys := map[string]struct{}{
		captchaFlightKey(mobile, fixedA): {},
		captchaFlightKey(mobile, fixedB): {},
		captchaFlightKey(mobile, fixedC): {},
	}
	if len(keys) != 3 {
		t.Fatalf("flight key must be scoped by account+activity, got %v", keys)
	}
}

// 并发创建同一个 key 时只能有一个成功创建（其余必须复用）。
func TestCaptchaFlightStartConcurrent(t *testing.T) {
	c := newFlightClient()
	key := captchaFlightKey("13500000000", FixedParams{ActiveID: 1, UID: 2})

	const n = 32
	var created int32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, ok := c.captchaFlightStart(key); ok {
				atomic.AddInt32(&created, 1)
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("created %d flights, want exactly 1", created)
	}
}

// 网络层失败与"滑块位置不对"要区分开：
// 前者连续两次就应放弃（再试只是烧掉二维码有效期），后者才值得换候选重试。
func TestIsTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"business", errors.New("captcha verify failed"), false},
		{"jsonp", errors.New("invalid jsonp"), false},
		{"url error", &url.Error{Op: "Get", URL: "https://captcha.chaoxing.com/x", Err: errors.New("proxyconnect tcp: timeout")}, true},
		{"op error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection reset by peer")}, true},
		{"dns error", &net.DNSError{Err: "no such host"}, true},
		{"wrapped url error", fmt.Errorf("captcha transport failed: %w", &url.Error{Op: "Get", Err: context.DeadlineExceeded}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTransportError(c.err); got != c.want {
				t.Fatalf("isTransportError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
