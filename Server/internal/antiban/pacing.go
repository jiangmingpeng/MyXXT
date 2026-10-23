package antiban

import (
	"context"
	"sync"
	"time"
)

// pacingTier 描述一档节流参数。
type pacingTier struct {
	min time.Duration
	max time.Duration
}

// pacer 负责三件事：
//  1. 全局并发上限（避免同一时刻把 N 个账号的请求一起打出去）。
//  2. 账号级最小间隔 + 随机抖动（让同一账号的连续请求更像真人操作）。
//  3. 读/写分级：签到（写）严格限速，列表/详情（读）用更短的间隔，
//     否则一次活动列表首屏要串行几十个请求，等待时间会长到不可用。
//
// 实现上刻意"先睡再占坑"：如果持着并发槽位睡觉，会把别的账号一起堵住。
type pacer struct {
	sem chan struct{}

	write pacingTier
	read  pacingTier

	mu   sync.Mutex
	last map[string]time.Time
}

func newPacer(maxConcurrency int, read, write pacingTier) *pacer {
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	return &pacer{
		sem:   make(chan struct{}, maxConcurrency),
		write: write,
		read:  read,
		last:  make(map[string]time.Time),
	}
}

// acquire 返回 release；调用方必须 defer release()。
func (p *pacer) acquire(ctx context.Context, key string, tier pacingTier) (func(), error) {
	if p == nil {
		return func() {}, nil
	}

	// 读与写各自排队：真机也可能同时有列表刷新和一次提交，互不影响。
	wait := p.reserve(key, tier)
	if wait > 0 {
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, err
		}
	}

	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var once sync.Once
	return func() {
		once.Do(func() { <-p.sem })
	}, nil
}

// reserve 原子地计算出"这次需要等待多久"，并把账号的下次可用时间往后推。
func (p *pacer) reserve(key string, tier pacingTier) time.Duration {
	now := time.Now()
	gap := RandomDuration(tier.min, tier.max)

	p.mu.Lock()
	defer p.mu.Unlock()

	at, ok := p.last[key]
	if !ok {
		p.last[key] = now.Add(gap)
		return 0
	}
	if at.Before(now) {
		at = now
	}
	p.last[key] = at.Add(gap)
	return at.Sub(now)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
