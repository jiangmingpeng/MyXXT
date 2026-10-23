package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"xbt2/server/internal/antiban"
)

// AntibanDeviceTemplate 是 config.yaml 里的一台"真实手机"。
type AntibanDeviceTemplate struct {
	Model     string `yaml:"model"`
	UserAgent string `yaml:"user_agent"`
}

// AntibanConfig 是反风控（设备指纹 / 出口代理 / 节流 / 熔断）的配置段。
// 所有字段都有安全默认值，缺省时也能正常启动。
type AntibanConfig struct {
	// Enabled 关闭后不节流、不熔断（仅用于排障），但仍会稳定化设备指纹。
	// 用指针是为了区分"配置里没写"和"显式写 false"，避免显式关闭被默认值覆盖。
	Enabled *bool `yaml:"enabled"`

	// ProxyPool 是出口代理池，支持 http/https/socks5。
	// 留空表示直连服务器本机 IP（风险最高，仅建议本地调试）。
	ProxyPool []string `yaml:"proxy_pool"`

	// MaxConcurrency 是全局在途请求上限。
	MaxConcurrency int `yaml:"max_concurrency"`
	// MinIntervalMS/MaxIntervalMS 是同一账号连续"写"请求（登录/预签到/签到）的随机间隔。
	MinIntervalMS int `yaml:"min_interval_ms"`
	MaxIntervalMS int `yaml:"max_interval_ms"`

	// ReadMinIntervalMS/ReadMaxIntervalMS 是同一账号连续"读"请求（活动列表/详情）的随机间隔。
	// 读请求量大但不改变账号状态，间隔必须比写请求短，否则活动列表首屏会慢到不可用。
	ReadMinIntervalMS int `yaml:"read_min_interval_ms"`
	ReadMaxIntervalMS int `yaml:"read_max_interval_ms"`

	// CaptchaMaxAttempts 是单次滑动验证码最多尝试的位置数。
	CaptchaMaxAttempts int `yaml:"captcha_max_attempts"`

	// BreakerThreshold/BreakerCooldownSec 控制账号级熔断。
	BreakerThreshold   float64 `yaml:"breaker_threshold"`
	BreakerCooldownSec int     `yaml:"breaker_cooldown_sec"`

	// DeviceTemplates 是设备指纹池；为空时所有账号共用 chaoxing_user_agent。
	DeviceTemplates []AntibanDeviceTemplate `yaml:"device_templates"`

	// UnifyLoginUA 让登录请求也使用移动端 UA，避免"登录用浏览器、签到用 App"的割裂。
	UnifyLoginUA *bool `yaml:"unify_login_ua"`

	// ClientIPMode 控制 stuSignajax 的 clientip 参数：
	//   empty（默认，保持历史行为）| auto（经代理探测出口 IP）| fixed（使用 client_ip）
	ClientIPMode string `yaml:"client_ip_mode"`
	ClientIP     string `yaml:"client_ip"`
	// ExitIPProbeURL 是探测出口 IP 的服务，默认 https://ifconfig.me/ip。
	ExitIPProbeURL string `yaml:"exit_ip_probe_url"`

	// LogPayloads 打开后会打印 enc/验签参数等敏感内容（默认关闭）。
	LogPayloads bool `yaml:"log_payloads"`
}

type Config struct {
	AppEnv            string        `yaml:"app_env"`
	HTTPAddr          string        `yaml:"http_addr"`
	JWTSecret         string        `yaml:"jwt_secret"`
	CredentialSecret  string        `yaml:"credential_secret"`
	AllowInsecureTLS  bool          `yaml:"allow_insecure_tls"`
	ChaoxingAESKey    string        `yaml:"chaoxing_aes_key"`
	ChaoxingUserAgent string        `yaml:"chaoxing_user_agent"`
	ActivityListLimit int           `yaml:"activity_list_limit"`
	PostgresDSN       string        `yaml:"postgres_dsn"`
	Antiban           AntibanConfig `yaml:"antiban"`
}

func Load() Config {
	cfg := Config{}

	raw, err := os.ReadFile("config.yaml")
	if err != nil {
		panic(fmt.Errorf("read config.yaml failed: %w", err))
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		panic(fmt.Errorf("parse config.yaml failed: %w", err))
	}

	if cfg.ActivityListLimit <= 0 {
		cfg.ActivityListLimit = 5
	}
	cfg.Antiban = withAntibanDefaults(cfg.Antiban)
	return cfg
}

// boolOrDefault 用于 *bool 配置项：nil 表示"未配置"，返回默认值。
func boolOrDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// IsEnabled 返回反风控总开关（默认开启）。
func (a AntibanConfig) IsEnabled() bool {
	return boolOrDefault(a.Enabled, true)
}

// IsUnifyLoginUA 返回是否统一登录 UA（默认开启）。
func (a AntibanConfig) IsUnifyLoginUA() bool {
	return boolOrDefault(a.UnifyLoginUA, true)
}

// withAntibanDefaults 补齐缺省值，并把非法配置夹到安全区间。
func withAntibanDefaults(a AntibanConfig) AntibanConfig {
	if a.MaxConcurrency <= 0 {
		a.MaxConcurrency = 2
	}
	if a.MaxConcurrency > 8 {
		a.MaxConcurrency = 8
	}
	if a.MinIntervalMS <= 0 {
		a.MinIntervalMS = 300
	}
	if a.MaxIntervalMS < a.MinIntervalMS {
		a.MaxIntervalMS = a.MinIntervalMS * 3
	}
	if a.ReadMinIntervalMS <= 0 {
		a.ReadMinIntervalMS = 80
	}
	if a.ReadMaxIntervalMS < a.ReadMinIntervalMS {
		a.ReadMaxIntervalMS = a.ReadMinIntervalMS * 3
	}
	// 读间隔不能超过写间隔，否则"分级"反而让读请求更慢。
	if a.ReadMaxIntervalMS > a.MinIntervalMS {
		a.ReadMaxIntervalMS = a.MinIntervalMS
	}
	if a.ReadMinIntervalMS > a.ReadMaxIntervalMS {
		a.ReadMinIntervalMS = a.ReadMaxIntervalMS
	}
	if a.CaptchaMaxAttempts <= 0 {
		a.CaptchaMaxAttempts = 8
	}
	if a.CaptchaMaxAttempts > 20 {
		a.CaptchaMaxAttempts = 20
	}
	if a.BreakerThreshold <= 0 {
		a.BreakerThreshold = 4
	}
	if a.BreakerCooldownSec <= 0 {
		a.BreakerCooldownSec = 90
	}
	switch strings.ToLower(strings.TrimSpace(a.ClientIPMode)) {
	case "auto":
		a.ClientIPMode = "auto"
	case "fixed":
		a.ClientIPMode = "fixed"
	default:
		a.ClientIPMode = "empty"
	}
	if a.ClientIPMode == "fixed" && strings.TrimSpace(a.ClientIP) == "" {
		a.ClientIPMode = "empty"
	}

	pool := make([]string, 0, len(a.ProxyPool))
	for _, raw := range a.ProxyPool {
		if v := strings.TrimSpace(raw); v != "" {
			pool = append(pool, v)
		}
	}
	a.ProxyPool = pool

	templates := make([]AntibanDeviceTemplate, 0, len(a.DeviceTemplates))
	for _, t := range a.DeviceTemplates {
		if strings.TrimSpace(t.UserAgent) == "" {
			continue
		}
		templates = append(templates, t)
	}
	a.DeviceTemplates = templates
	return a
}

// ToAntibanOptions 把配置转换为引擎参数。
func (a AntibanConfig) ToAntibanOptions(fallbackUA string) antiban.Options {
	templates := make([]antiban.DeviceTemplate, 0, len(a.DeviceTemplates))
	for _, t := range a.DeviceTemplates {
		templates = append(templates, antiban.DeviceTemplate{Model: t.Model, UserAgent: t.UserAgent})
	}
	return antiban.Options{
		Enabled:            a.IsEnabled(),
		ProxyPool:          a.ProxyPool,
		MaxConcurrency:     a.MaxConcurrency,
		MinInterval:        time.Duration(a.MinIntervalMS) * time.Millisecond,
		MaxInterval:        time.Duration(a.MaxIntervalMS) * time.Millisecond,
		ReadMinInterval:    time.Duration(a.ReadMinIntervalMS) * time.Millisecond,
		ReadMaxInterval:    time.Duration(a.ReadMaxIntervalMS) * time.Millisecond,
		CaptchaMaxAttempts: a.CaptchaMaxAttempts,
		BreakerThreshold:   a.BreakerThreshold,
		BreakerCooldown:    time.Duration(a.BreakerCooldownSec) * time.Second,
		DeviceTemplates:    templates,
		FallbackUserAgent:  fallbackUA,
	}
}

func (c Config) MaskedDSN() string {
	return fmt.Sprintf("%s ...", c.PostgresDSN[:min(len(c.PostgresDSN), 24)])
}
