package xxt

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// captchaFlight 是一次"单飞"的验证码求解。
//
// 背景：二维码签到的 enc 只能用一次，首发 stuSignajax 必须直接带 validate，
// 所以每次签到都要解一次滑动验证码；而旧实现里"并行预取"与 Sign 内部兜底
// 是两次独立调用——只要预取失败（代理抖动很常见），Sign 就会把整套验证码
// 再解一遍，单次签到耗时直接翻倍，很容易超过二维码有效期。
//
// 单飞之后：同一活动 + 同一账号在同一时刻只会真正请求验证码服务一次，
// Sign 需要 validate 时直接等这一个结果，不再重复求解。
type captchaFlight struct {
	done chan struct{}
	val  string
	err  error
}

// captchaFlightKey 以"账号 + 活动"为粒度。
// 不同活动的验证码不能混用；同一活动的不同账号出口 IP 不同，
// validate 也不能跨账号共享。
func captchaFlightKey(mobile string, fixed FixedParams) string {
	return fmt.Sprintf("%s|%d|%d", mobile, fixed.ActiveID, fixed.UID)
}

// captchaFlightStart 返回（必要时创建）该 key 的在途任务。
// created=true 表示调用方负责真正执行求解并回填结果。
func (c *Client) captchaFlightStart(key string) (fl *captchaFlight, created bool) {
	c.captchaMu.Lock()
	defer c.captchaMu.Unlock()
	if c.captchaFlights == nil {
		// 测试会直接构造 &Client{}，这里必须惰性初始化。
		c.captchaFlights = make(map[string]*captchaFlight)
	}
	if existing, ok := c.captchaFlights[key]; ok {
		return existing, false
	}
	fl = &captchaFlight{done: make(chan struct{})}
	c.captchaFlights[key] = fl
	return fl, true
}

// captchaFlightFinish 写入结果并唤醒所有等待者。
// 结果保留一小段时间供紧接着的 Sign 复用，之后自清理防止泄漏。
func (c *Client) captchaFlightFinish(key string, fl *captchaFlight, val string, err error) {
	fl.val, fl.err = val, err
	close(fl.done)
	time.AfterFunc(30*time.Second, func() {
		c.captchaFlightConsume(key, fl)
	})
}

// captchaFlightPeek 返回在途/已完成的任务，不消费。
func (c *Client) captchaFlightPeek(key string) *captchaFlight {
	c.captchaMu.Lock()
	defer c.captchaMu.Unlock()
	return c.captchaFlights[key]
}

// captchaFlightConsume 取走结果并清理，保证同一个 validate 只会被使用一次。
func (c *Client) captchaFlightConsume(key string, fl *captchaFlight) {
	c.captchaMu.Lock()
	if c.captchaFlights[key] == fl {
		delete(c.captchaFlights, key)
	}
	c.captchaMu.Unlock()
}

// StartCaptchaPrefetch 在后台开始抓一次验证码，立即返回。
// 已有同 key 的在途任务时直接复用，不会重复打扰验证码服务。
func (c *Client) StartCaptchaPrefetch(mobile string, fixed FixedParams) {
	if c == nil {
		return
	}
	key := captchaFlightKey(mobile, fixed)
	fl, created := c.captchaFlightStart(key)
	if !created {
		return
	}
	go func() {
		started := time.Now()
		val, err := c.fetchCaptchaValidate(mobile, fixed)
		log.Printf("[captcha] prefetch activity=%d uid=%d elapsed=%s ok=%t err=%v",
			fixed.ActiveID, fixed.UID, time.Since(started).Round(time.Millisecond),
			err == nil && strings.TrimSpace(val) != "", err)
		c.captchaFlightFinish(key, fl, val, err)
	}()
}

// TakePrefetchedCaptcha 非阻塞取预取结果。
// ok=false 表示还没有结果（仍在途或已失败），调用方应直接继续，
// 由 xxt.Sign 内部去等待同一个在途任务。
//
// 只消费"成功且非空"的结果：失败结果必须留给 Sign 的兜底去消费，
// 否则 Sign 会把整套验证码重新解一遍——那正是耗时翻倍的老问题。
func (c *Client) TakePrefetchedCaptcha(mobile string, fixed FixedParams) (string, bool) {
	if c == nil {
		return "", false
	}
	key := captchaFlightKey(mobile, fixed)
	fl := c.captchaFlightPeek(key)
	if fl == nil {
		return "", false
	}
	select {
	case <-fl.done:
	default:
		return "", false
	}
	if fl.err != nil {
		return "", false
	}
	v := strings.TrimSpace(fl.val)
	if v == "" {
		return "", false
	}
	c.captchaFlightConsume(key, fl)
	return v, true
}

// awaitCaptchaPrefetch 等待在途预取结果，没有在途任务时才自己抓一次。
//
// 注意：预取已经失败时不再重试。失败通常来自代理抖动，紧接着重试大概率还是失败，
// 却会把耗时翻倍直接顶穿二维码有效期；快速失败让用户重新扫一张新码更划算。
func (c *Client) awaitCaptchaPrefetch(mobile string, fixed FixedParams) (string, error) {
	key := captchaFlightKey(mobile, fixed)
	fl := c.captchaFlightPeek(key)
	if fl == nil {
		return c.fetchCaptchaValidate(mobile, fixed)
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-fl.done:
	case <-timer.C:
		return "", fmt.Errorf("captcha prefetch timeout")
	}
	c.captchaFlightConsume(key, fl)
	return strings.TrimSpace(fl.val), fl.err
}
