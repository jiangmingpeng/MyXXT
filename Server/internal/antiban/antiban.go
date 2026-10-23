// Package antiban 把"让自动化请求更像真实学生手机"的工程约束集中到一处：
//
//  1. 设备指纹：每个账号绑定一台稳定设备（User-Agent / deviceCode 不随登录漂移）。
//  2. 出口代理：按账号稳定分配代理，避免全部账号共用一个数据中心 IP。
//  3. 请求节流：全局并发上限 + 账号级最小间隔 + 随机抖动，消除"同一毫秒打一堆请求"。
//  4. 风控熔断：识别"请求过于频繁/需要验证码/账号异常"等信号，自动冷却该账号。
//
// 该包刻意不依赖 xxt / gorm，只暴露窄接口（Guard / Store），
// 避免和业务包产生循环引用，也方便单独写单元测试。
package antiban

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// Signal 表示一次与学习通交互后观察到的健康/风控信号。
type Signal int

const (
	// SignalNone 表示没有可判定的信号。
	SignalNone Signal = iota
	// SignalSuccess 表示请求成功，可以清空风险计数。
	SignalSuccess
	// SignalTransient 表示网络抖动/5xx 等瞬时故障。
	SignalTransient
	// SignalLocationRejected 表示定位校验未通过（属业务错误，不是风控）。
	SignalLocationRejected
	// SignalCaptcha 表示学习通要求人机校验，是最常见的"可疑"信号。
	SignalCaptcha
	// SignalRateLimited 表示请求过于频繁。
	SignalRateLimited
	// SignalBlocked 表示账号已被限制/存在风险。
	SignalBlocked
)

func (s Signal) String() string {
	switch s {
	case SignalSuccess:
		return "success"
	case SignalTransient:
		return "transient"
	case SignalLocationRejected:
		return "location_rejected"
	case SignalCaptcha:
		return "captcha"
	case SignalRateLimited:
		return "rate_limited"
	case SignalBlocked:
		return "blocked"
	default:
		return "none"
	}
}

// weight 是熔断累计权重。业务类错误不计入，风控类错误快速累计。
func (s Signal) weight() float64 {
	switch s {
	case SignalTransient:
		return 0.1
	case SignalCaptcha:
		return 0.5
	case SignalRateLimited:
		return 1
	case SignalBlocked:
		return 1.5
	default:
		return 0
	}
}

// Profile 是分配给某个账号的稳定"设备 + 出口"画像。
// 同一账号在任何一次登录/签到里都应该拿到完全相同的画像，
// 否则设备指纹漂移本身就是一条风控信号。
type Profile struct {
	Mobile      string
	UserAgent   string
	DeviceCode  string
	DeviceModel string
	// ProxyURL 为空表示直连（走服务器本机出口）。
	ProxyURL string
	// ExitIP 是代理出口 IP，仅用于按需填充 clientip 参数。
	ExitIP string
}

// Store 持久化账号画像，确保跨进程重启后设备指纹不漂移。
// 实现方通常是数据库；返回 false 表示该账号还没有画像。
type Store interface {
	Load(mobile string) (Profile, bool)
	Save(p Profile) error
}

// DeviceTemplate 是一台"真实手机"的完整指纹模板。
type DeviceTemplate struct {
	Model     string
	UserAgent string
}

// Options 是反风控引擎的运行参数，全部来自 config.yaml 的 antiban 段。
type Options struct {
	// Enabled 关闭后引擎退化为 no-op（不节流、不熔断），
	// 但设备指纹仍会稳定化，方便排障时对比行为差异。
	Enabled bool

	// ProxyPool 是可用出口列表，支持 http/https/socks5，
	// 形如 "http://user:pass@host:port"。同一账号固定用其中一个。
	ProxyPool []string

	// MaxConcurrency 是同一时刻最多允许在途的学习通请求数（全局）。
	MaxConcurrency int
	// MinInterval/MaxInterval 是同一账号两次"写"请求（登录/预签到/签到）之间的随机间隔。
	MinInterval time.Duration
	MaxInterval time.Duration
	// ReadMinInterval/ReadMaxInterval 是同一账号两次"读"请求（活动列表/详情）之间的随机间隔。
	// 读请求量大且不改变账号状态，间隔必须比写请求短，否则首屏会慢到不可用。
	ReadMinInterval time.Duration
	ReadMaxInterval time.Duration

	// CaptchaMaxAttempts 是单次验证码最多尝试的滑块位置个数。
	// 学习通验证码服务本身有风控，暴力枚举是最高危的行为之一。
	CaptchaMaxAttempts int

	// BreakerThreshold 是风险权重累计到多少后熔断该账号。
	BreakerThreshold float64
	// BreakerCooldown 是熔断基础冷却时长，重复熔断会指数增长。
	BreakerCooldown time.Duration

	// DeviceTemplates 是设备指纹池，为空时所有账号沿用 FallbackUserAgent。
	DeviceTemplates []DeviceTemplate
	// FallbackUserAgent 是未配置指纹池时使用的 UA（兼容旧配置）。
	FallbackUserAgent string
}

// Status 描述某个账号当前的风控状态，供接口层返回给前端展示。
type Status struct {
	Blocked         bool
	Remaining       time.Duration
	RiskScore       float64
	LastSignal      Signal
	ConsecutiveOpen int
}

// BlockedError 表示该账号正处于熔断冷却期，调用方应直接失败而不是继续重试。
type BlockedError struct {
	Mobile    string
	Remaining time.Duration
}

func (e *BlockedError) Error() string {
	sec := int(e.Remaining.Seconds())
	if sec < 1 {
		sec = 1
	}
	return fmt.Sprintf("该账号触发风控保护，请在 %d 秒后重试", sec)
}

// Guard 是 xxt 包唯一依赖的窄接口。
// 传 nil 表示完全不启用反风控（用于单元测试和降级）。
type Guard interface {
	// ProfileFor 返回该账号稳定的设备指纹与出口代理。
	ProfileFor(mobile string) Profile
	// Acquire 申请一次"写"请求的出站许可（登录/预签到/签到）：
	// 内部完成账号级节流与全局并发控制，返回的 release 必须在请求结束后调用。
	Acquire(ctx context.Context, mobile string) (release func(), err error)
	// AcquireRead 申请一次"读"请求的出站许可（活动列表/详情）。
	// 与写请求共用全局并发上限，但账号级间隔更短。
	AcquireRead(ctx context.Context, mobile string) (release func(), err error)
	// Observe 上报一次请求结果，用于熔断状态机。
	Observe(mobile string, sig Signal)
	// Status 返回账号当前风控状态。
	Status(mobile string) Status
	// CaptchaMaxAttempts 返回单次验证码允许的最大试错次数。
	CaptchaMaxAttempts() int
	// Enabled 表示节流/熔断是否真正生效。
	Enabled() bool
}

// Engine 是 Guard 的默认实现。
type Engine struct {
	opts  Options
	store Store
	pacer *pacer

	mu       sync.Mutex
	profiles map[string]Profile
	breakers map[string]*breaker
}

// New 构造引擎。store 可以为 nil（画像只留在内存里，进程重启后按规则重新派生）。
func New(opts Options, store Store) *Engine {
	opts = normalizeOptions(opts)
	return &Engine{
		opts:  opts,
		store: store,
		pacer: newPacer(
			opts.MaxConcurrency,
			pacingTier{min: opts.ReadMinInterval, max: opts.ReadMaxInterval},
			pacingTier{min: opts.MinInterval, max: opts.MaxInterval},
		),
		profiles: make(map[string]Profile),
		breakers: make(map[string]*breaker),
	}
}

func normalizeOptions(o Options) Options {
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = 2
	}
	if o.MinInterval <= 0 {
		o.MinInterval = 300 * time.Millisecond
	}
	if o.MaxInterval < o.MinInterval {
		o.MaxInterval = o.MinInterval
	}
	if o.ReadMinInterval <= 0 {
		o.ReadMinInterval = 80 * time.Millisecond
	}
	if o.ReadMaxInterval < o.ReadMinInterval {
		o.ReadMaxInterval = 240 * time.Millisecond
	}
	if o.ReadMaxInterval > o.MinInterval {
		// 读间隔不应超过写间隔，否则分级就失去意义。
		o.ReadMaxInterval = o.MinInterval
	}
	if o.ReadMinInterval > o.ReadMaxInterval {
		o.ReadMinInterval = o.ReadMaxInterval
	}
	if o.CaptchaMaxAttempts <= 0 {
		o.CaptchaMaxAttempts = 8
	}
	if o.BreakerThreshold <= 0 {
		o.BreakerThreshold = 4
	}
	if o.BreakerCooldown <= 0 {
		o.BreakerCooldown = 90 * time.Second
	}
	pool := make([]string, 0, len(o.ProxyPool))
	for _, raw := range o.ProxyPool {
		if v := strings.TrimSpace(raw); v != "" {
			pool = append(pool, v)
		}
	}
	o.ProxyPool = pool
	return o
}

// Enabled 实现 Guard。
func (e *Engine) Enabled() bool {
	return e != nil && e.opts.Enabled
}

// CaptchaMaxAttempts 实现 Guard。
func (e *Engine) CaptchaMaxAttempts() int {
	if e == nil {
		return 8
	}
	return e.opts.CaptchaMaxAttempts
}

// Options 返回引擎生效中的配置（只读副本），便于启动日志打印。
func (e *Engine) Options() Options {
	if e == nil {
		return Options{}
	}
	return e.opts
}

// ProfileFor 实现 Guard：返回（并缓存、持久化）账号的稳定画像。
func (e *Engine) ProfileFor(mobile string) Profile {
	if e == nil {
		return Profile{Mobile: mobile}
	}
	mobile = strings.TrimSpace(mobile)
	e.mu.Lock()
	if p, ok := e.profiles[mobile]; ok {
		e.mu.Unlock()
		return p
	}
	e.mu.Unlock()

	p := e.buildProfile(mobile)

	e.mu.Lock()
	e.profiles[mobile] = p
	e.mu.Unlock()

	// 只在新派生时落库，避免每次请求都写数据库。
	if e.store != nil {
		if err := e.store.Save(p); err != nil {
			log.Printf("[antiban] persist profile failed mobile=%s err=%v", maskMobile(mobile), err)
		}
	}
	return p
}

func (e *Engine) buildProfile(mobile string) Profile {
	p := Profile{Mobile: mobile, UserAgent: e.opts.FallbackUserAgent}

	if e.store != nil {
		if stored, ok := e.store.Load(mobile); ok {
			if v := strings.TrimSpace(stored.UserAgent); v != "" {
				p.UserAgent = v
			}
			if v := strings.TrimSpace(stored.DeviceCode); v != "" {
				p.DeviceCode = v
			}
			if v := strings.TrimSpace(stored.DeviceModel); v != "" {
				p.DeviceModel = v
			}
			if v := strings.TrimSpace(stored.ProxyURL); v != "" {
				p.ProxyURL = v
			}
			if v := strings.TrimSpace(stored.ExitIP); v != "" {
				p.ExitIP = v
			}
		}
	}

	// 指纹池：只有"还没有任何指纹"的账号才从池里挑一台，保证长期稳定。
	if p.DeviceModel == "" && len(e.opts.DeviceTemplates) > 0 {
		tpl := e.opts.DeviceTemplates[StickyIndex(mobile, len(e.opts.DeviceTemplates))]
		p.DeviceModel = tpl.Model
		if strings.TrimSpace(tpl.UserAgent) != "" {
			p.UserAgent = tpl.UserAgent
		}
	}
	if p.DeviceModel == "" {
		p.DeviceModel = DeviceModelFromUA(p.UserAgent)
	}
	// deviceCode 必须"账号级稳定"：真机的设备标识不会每次登录都变。
	if p.DeviceCode == "" {
		p.DeviceCode = StableDeviceCode(mobile, p.DeviceModel)
	}
	if len(e.opts.ProxyPool) > 0 && p.ProxyURL == "" {
		p.ProxyURL = e.opts.ProxyPool[StickyIndex(mobile, len(e.opts.ProxyPool))]
	}
	return p
}

// Acquire 实现 Guard：熔断检查 + 账号节流 + 全局并发控制（写请求）。
func (e *Engine) Acquire(ctx context.Context, mobile string) (func(), error) {
	if e == nil || !e.opts.Enabled {
		return func() {}, nil
	}
	if st := e.Status(mobile); st.Blocked {
		return nil, &BlockedError{Mobile: mobile, Remaining: st.Remaining}
	}
	return e.pacer.acquire(ctx, mobile+"|w", e.pacer.write)
}

// AcquireRead 实现 Guard：读请求共享并发上限，但使用更短的账号级间隔。
func (e *Engine) AcquireRead(ctx context.Context, mobile string) (func(), error) {
	if e == nil || !e.opts.Enabled {
		return func() {}, nil
	}
	if st := e.Status(mobile); st.Blocked {
		return nil, &BlockedError{Mobile: mobile, Remaining: st.Remaining}
	}
	return e.pacer.acquire(ctx, mobile+"|r", e.pacer.read)
}

// Observe 实现 Guard：把一次请求结果喂给熔断状态机。
func (e *Engine) Observe(mobile string, sig Signal) {
	if e == nil || !e.opts.Enabled || sig == SignalNone {
		return
	}
	b := e.breakerFor(mobile)
	cooldown, tripped := b.observe(sig, e.opts.BreakerThreshold, e.opts.BreakerCooldown)
	if tripped {
		log.Printf("[antiban] breaker open mobile=%s signal=%s cooldown=%s", maskMobile(mobile), sig, cooldown)
	}
}

// Status 实现 Guard。
func (e *Engine) Status(mobile string) Status {
	if e == nil {
		return Status{}
	}
	return e.breakerFor(mobile).status()
}

func (e *Engine) breakerFor(mobile string) *breaker {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.breakers[mobile]
	if !ok {
		b = newBreaker()
		e.breakers[mobile] = b
	}
	return b
}

// ProxyFor 便捷方法：返回账号绑定的出口代理（可能为空）。
func (e *Engine) ProxyFor(mobile string) string {
	return e.ProfileFor(mobile).ProxyURL
}

// SetExitIP 记录代理的实际出口 IP，供 clientip 参数按需使用。
func (e *Engine) SetExitIP(mobile, ip string) {
	ip = strings.TrimSpace(ip)
	if e == nil || ip == "" {
		return
	}
	// 先保证画像已派生：否则会把一条 UA/deviceCode 为空的记录写进库。
	p := e.ProfileFor(mobile)
	p.ExitIP = ip

	e.mu.Lock()
	e.profiles[mobile] = p
	e.mu.Unlock()

	if e.store != nil {
		if err := e.store.Save(p); err != nil {
			log.Printf("[antiban] persist exit ip failed mobile=%s err=%v", maskMobile(mobile), err)
		}
	}
}

// UIDBinder 是 Store 的可选能力：支持把画像与真实 uid 关联。
// 引擎在登录成功后调用，便于后台按 uid 排查设备指纹。
type UIDBinder interface {
	BindUID(mobile string, uid int64) error
}

// BindUID 实现 xxt 包可选依赖的 binder 接口。
func (e *Engine) BindUID(mobile string, uid int64) {
	if e == nil || e.store == nil || uid <= 0 {
		return
	}
	b, ok := e.store.(UIDBinder)
	if !ok {
		return
	}
	if err := b.BindUID(mobile, uid); err != nil {
		log.Printf("[antiban] bind uid failed mobile=%s uid=%d err=%v", maskMobile(mobile), uid, err)
	}
}

// RandomDuration 返回 [min,max] 之间的随机时长。
func RandomDuration(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int64N(int64(max-min)+1))
}

// StickyIndex 把 key 稳定地映射到 [0,n)。
// 用 FNV 而不是随机数，保证同一账号永远落在同一个槽位。
func StickyIndex(key string, n int) int {
	if n <= 1 {
		return 0
	}
	return int(fnv1a(key) % uint64(n))
}

func fnv1a(s string) uint64 {
	const (
		offset64 = 1469598103934665603
		prime64  = 1099511628211
	)
	var h uint64 = offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// maskMobile 用于日志脱敏：138****8888。
func maskMobile(mobile string) string {
	if len(mobile) < 7 {
		return "***"
	}
	return mobile[:3] + "****" + mobile[len(mobile)-4:]
}
