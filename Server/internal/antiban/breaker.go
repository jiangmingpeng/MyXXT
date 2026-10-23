package antiban

import (
	"sync"
	"time"
)

// breaker 是账号级熔断器。
//
// 设计目标：当学习通反复给出"请求过于频繁 / 需要验证码 / 账号异常"时，
// 继续硬刚只会把账号彻底打黑，正确做法是立刻停下来冷静一段时间。
type breaker struct {
	mu        sync.Mutex
	risk      float64
	openUntil time.Time
	opened    int
	last      Signal
}

func newBreaker() *breaker {
	return &breaker{}
}

// observe 返回 (本次冷却时长, 是否刚刚触发熔断)。
func (b *breaker) observe(sig Signal, threshold float64, base time.Duration) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.last = sig

	// 成功即清零：说明账号恢复正常，之前累计的风险不再有意义。
	if sig == SignalSuccess {
		b.risk = 0
		b.opened = 0
		return 0, false
	}

	if sig == SignalLocationRejected {
		// 定位不通过是业务错误，不该影响风控状态。
		return 0, false
	}

	b.risk += sig.weight()
	if b.risk < threshold {
		return 0, false
	}

	// 指数增长：连续熔断说明账号确实被盯上了，冷却时间要越来越长。
	b.opened++
	cooldown := base
	for i := 1; i < b.opened && cooldown < 30*time.Minute; i++ {
		cooldown *= 2
	}
	if cooldown > 30*time.Minute {
		cooldown = 30 * time.Minute
	}
	b.openUntil = time.Now().Add(cooldown)
	b.risk = 0
	return cooldown, true
}

func (b *breaker) status() Status {
	if b == nil {
		return Status{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	st := Status{
		RiskScore:       b.risk,
		LastSignal:      b.last,
		ConsecutiveOpen: b.opened,
	}
	if rem := time.Until(b.openUntil); rem > 0 {
		st.Blocked = true
		st.Remaining = rem
	}
	return st
}
