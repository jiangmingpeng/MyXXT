package antiban

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore 是内存版 Store，用于验证画像持久化与复用。
type fakeStore struct {
	mu   sync.Mutex
	data map[string]Profile
	save int32
}

func newFakeStore() *fakeStore {
	return &fakeStore{data: make(map[string]Profile)}
}

func (s *fakeStore) Load(mobile string) (Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data[mobile]
	return p, ok
}

func (s *fakeStore) Save(p Profile) error {
	atomic.AddInt32(&s.save, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[p.Mobile] = p
	return nil
}

func testOptions() Options {
	return Options{
		Enabled:            true,
		MaxConcurrency:     2,
		MinInterval:        20 * time.Millisecond,
		MaxInterval:        30 * time.Millisecond,
		CaptchaMaxAttempts: 5,
		BreakerThreshold:   4,
		BreakerCooldown:    50 * time.Millisecond,
		FallbackUserAgent:  "Dalvik/2.1.0 (Linux; U; Android 14; Redmi K20 Pro Build/UKQ1.230804.001)",
	}
}

func TestStableDeviceCodeIsDeterministic(t *testing.T) {
	a := StableDeviceCode("13800000001", "Redmi K20 Pro")
	b := StableDeviceCode("13800000001", "Redmi K20 Pro")
	c := StableDeviceCode("13800000002", "Redmi K20 Pro")

	if a != b {
		t.Fatalf("同一账号应得到相同 deviceCode: %s != %s", a, b)
	}
	if a == c {
		t.Fatalf("不同账号不应共用 deviceCode")
	}
	if len(a) != 32 {
		t.Fatalf("deviceCode 应兼容 32 位 hex，实际 %d", len(a))
	}
	if strings.ToLower(a) != a {
		t.Fatalf("deviceCode 应为小写 hex: %s", a)
	}
}

func TestDeviceModelFromUA(t *testing.T) {
	ua := "Dalvik/2.1.0 (Linux; U; Android 14; Redmi K20 Pro Build/UKQ1.230804.001) (schild:abc) (device:Redmi K20 Pro) Language/zh_CN"
	if got := DeviceModelFromUA(ua); got != "Redmi K20 Pro" {
		t.Fatalf("解析机型失败: %q", got)
	}
	if got := DeviceModelFromUA("curl/8.0"); got != "Android" {
		t.Fatalf("解析不到时应回退 Android，实际 %q", got)
	}
}

func TestProfileForIsStableAndPersisted(t *testing.T) {
	store := newFakeStore()
	engine := New(testOptions(), store)

	first := engine.ProfileFor("13800000001")
	second := engine.ProfileFor("13800000001")

	if first.DeviceCode != second.DeviceCode || first.UserAgent != second.UserAgent {
		t.Fatalf("同一账号画像必须稳定: %+v vs %+v", first, second)
	}
	if first.DeviceCode == "" || first.UserAgent == "" {
		t.Fatalf("画像字段不应为空: %+v", first)
	}
	if got := atomic.LoadInt32(&store.save); got != 1 {
		t.Fatalf("画像应只落库一次，实际 %d", got)
	}

	// 新进程（新引擎 + 同一个 store）必须复用已落库的指纹。
	engine2 := New(testOptions(), store)
	third := engine2.ProfileFor("13800000001")
	if third.DeviceCode != first.DeviceCode || third.UserAgent != first.UserAgent {
		t.Fatalf("重启后指纹漂移: %+v vs %+v", third, first)
	}
}

func TestProfileAssignsStickyProxy(t *testing.T) {
	opts := testOptions()
	opts.ProxyPool = []string{"http://a:1", "http://b:2", "http://c:3"}
	engine := New(opts, newFakeStore())

	seen := map[string]string{}
	for _, mobile := range []string{"13800000001", "13800000002", "13800000003", "13800000001"} {
		p := engine.ProfileFor(mobile)
		if p.ProxyURL == "" {
			t.Fatalf("配置了代理池却没有分配代理: %s", mobile)
		}
		if prev, ok := seen[mobile]; ok && prev != p.ProxyURL {
			t.Fatalf("同一账号代理发生漂移: %s %s -> %s", mobile, prev, p.ProxyURL)
		}
		seen[mobile] = p.ProxyURL
	}
}

func TestPacerEnforcesPerAccountInterval(t *testing.T) {
	p := newPacer(4, pacingTier{60 * time.Millisecond, 60 * time.Millisecond}, pacingTier{60 * time.Millisecond, 60 * time.Millisecond})
	ctx := context.Background()

	start := time.Now()
	release, err := p.acquire(ctx, "13800000001|w", p.write)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	release()

	release2, err := p.acquire(ctx, "13800000001|w", p.write)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	release2()

	if elapsed := time.Since(start); elapsed < 55*time.Millisecond {
		t.Fatalf("同一账号两次请求间隔过短: %s", elapsed)
	}
}

func TestPacerReadTierIsFasterThanWrite(t *testing.T) {
	// 写请求 100ms 间隔，读请求 0 间隔：三次读应该很快跑完，写必须等待。
	p := newPacer(4, pacingTier{0, 0}, pacingTier{100 * time.Millisecond, 100 * time.Millisecond})
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 3; i++ {
		release, err := p.acquire(ctx, "13800000001|r", p.read)
		if err != nil {
			t.Fatalf("read acquire failed: %v", err)
		}
		release()
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("读请求不应被写间隔拖慢，耗时 %s", elapsed)
	}

	writeStart := time.Now()
	release, err := p.acquire(ctx, "13800000001|w", p.write)
	if err != nil {
		t.Fatalf("write acquire failed: %v", err)
	}
	release()
	release2, err := p.acquire(ctx, "13800000001|w", p.write)
	if err != nil {
		t.Fatalf("write acquire failed: %v", err)
	}
	release2()
	if elapsed := time.Since(writeStart); elapsed < 95*time.Millisecond {
		t.Fatalf("写请求必须遵守 100ms 间隔，实际 %s", elapsed)
	}
}

func TestPacerCapsGlobalConcurrency(t *testing.T) {
	p := newPacer(2, pacingTier{0, 0}, pacingTier{0, 0})
	ctx := context.Background()

	var inFlight int32
	var maxSeen int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := p.acquire(ctx, "same|w", p.write)
			if err != nil {
				t.Errorf("acquire failed: %v", err)
				return
			}
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				prev := atomic.LoadInt32(&maxSeen)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxSeen, prev, cur) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			release()
		}()
	}
	wg.Wait()

	if maxSeen > 2 {
		t.Fatalf("并发上限被突破: %d", maxSeen)
	}
}
