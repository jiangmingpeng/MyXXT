package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestAntibanDefaultsWhenSectionMissing(t *testing.T) {
	a := withAntibanDefaults(AntibanConfig{})

	if !a.IsEnabled() {
		t.Fatalf("缺省应默认开启反风控")
	}
	if !a.IsUnifyLoginUA() {
		t.Fatalf("缺省应默认统一登录 UA")
	}
	if a.MaxConcurrency != 2 {
		t.Fatalf("默认并发上限应为 2，实际 %d", a.MaxConcurrency)
	}
	if a.MinIntervalMS != 300 || a.MaxIntervalMS != 900 {
		t.Fatalf("默认间隔异常: %d~%d", a.MinIntervalMS, a.MaxIntervalMS)
	}
	if a.ReadMinIntervalMS != 80 || a.ReadMaxIntervalMS != 240 {
		t.Fatalf("默认读间隔异常: %d~%d", a.ReadMinIntervalMS, a.ReadMaxIntervalMS)
	}
	if a.ReadMaxIntervalMS > a.MinIntervalMS {
		t.Fatalf("读间隔不应大于写间隔: read=%d write=%d", a.ReadMaxIntervalMS, a.MinIntervalMS)
	}
	if a.CaptchaMaxAttempts != 8 {
		t.Fatalf("默认验证码尝试次数应为 8，实际 %d", a.CaptchaMaxAttempts)
	}
	if a.BreakerThreshold != 4 || a.BreakerCooldownSec != 90 {
		t.Fatalf("默认熔断参数异常: %v/%d", a.BreakerThreshold, a.BreakerCooldownSec)
	}
	if a.ClientIPMode != "empty" {
		t.Fatalf("默认 clientip 模式应为 empty，实际 %q", a.ClientIPMode)
	}
}

func TestExplicitDisableIsRespected(t *testing.T) {
	var a AntibanConfig
	if err := yaml.Unmarshal([]byte("enabled: false\nunify_login_ua: false\n"), &a); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	a = withAntibanDefaults(a)

	if a.IsEnabled() {
		t.Fatalf("显式 enabled:false 必须生效")
	}
	if a.IsUnifyLoginUA() {
		t.Fatalf("显式 unify_login_ua:false 必须生效")
	}
	// 其它字段仍应拿到默认值，方便随时重新开启。
	if a.MaxConcurrency != 2 {
		t.Fatalf("关闭后仍应补齐默认并发，实际 %d", a.MaxConcurrency)
	}
}

func TestAntibanClampsUnsafeValues(t *testing.T) {
	a := withAntibanDefaults(AntibanConfig{
		MaxConcurrency:     99,
		MinIntervalMS:      500,
		MaxIntervalMS:      100,
		CaptchaMaxAttempts: 500,
		ClientIPMode:       "whatever",
		ProxyPool:          []string{"  ", "http://a:1"},
		DeviceTemplates: []AntibanDeviceTemplate{
			{Model: "no-ua"},
			{Model: "ok", UserAgent: "ua"},
		},
	})

	if a.MaxConcurrency != 8 {
		t.Fatalf("并发上限应被夹到 8，实际 %d", a.MaxConcurrency)
	}
	if a.MaxIntervalMS < a.MinIntervalMS {
		t.Fatalf("最大间隔不应小于最小间隔: %d~%d", a.MinIntervalMS, a.MaxIntervalMS)
	}
	if a.CaptchaMaxAttempts != 20 {
		t.Fatalf("验证码尝试次数应被夹到 20，实际 %d", a.CaptchaMaxAttempts)
	}
	if a.ClientIPMode != "empty" {
		t.Fatalf("非法模式应回退 empty，实际 %q", a.ClientIPMode)
	}
	if len(a.ProxyPool) != 1 || a.ProxyPool[0] != "http://a:1" {
		t.Fatalf("空白代理应被过滤: %v", a.ProxyPool)
	}
	if len(a.DeviceTemplates) != 1 || a.DeviceTemplates[0].UserAgent != "ua" {
		t.Fatalf("无 UA 的模板应被丢弃: %v", a.DeviceTemplates)
	}
}

func TestClientIPFixedRequiresValue(t *testing.T) {
	a := withAntibanDefaults(AntibanConfig{ClientIPMode: "fixed"})
	if a.ClientIPMode != "empty" {
		t.Fatalf("fixed 模式缺少 client_ip 时应回退 empty，实际 %q", a.ClientIPMode)
	}

	b := withAntibanDefaults(AntibanConfig{ClientIPMode: "fixed", ClientIP: " 1.2.3.4 "})
	if b.ClientIPMode != "fixed" {
		t.Fatalf("提供了 client_ip 时应保持 fixed，实际 %q", b.ClientIPMode)
	}
}

func TestRepoConfigFileParses(t *testing.T) {
	// 直接解析仓库里真实的配置文件，防止文档与结构体定义脱节（例如拼错字段名）。
	// config.yaml 被 .gitignore 忽略，因此优先校验随仓库分发的 config.example.yaml。
	paths := []string{"../../config.example.yaml", "../../config.yaml"}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg Config
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("%s 解析失败: %v", p, err)
		}
		cfg.Antiban = withAntibanDefaults(cfg.Antiban)

		if !cfg.Antiban.IsEnabled() {
			t.Fatalf("%s 应启用反风控", p)
		}
		if len(cfg.Antiban.DeviceTemplates) == 0 {
			t.Fatalf("%s 应内置设备指纹池，否则所有账号仍是同一台手机", p)
		}
		for _, tpl := range cfg.Antiban.DeviceTemplates {
			if !strings.Contains(tpl.UserAgent, "com.chaoxing.mobile/ChaoXingStudy") {
				t.Fatalf("%s 的指纹 UA 格式不正确: %q", p, tpl.UserAgent)
			}
		}
		if cfg.AllowInsecureTLS {
			t.Fatalf("%s 应关闭 allow_insecure_tls", p)
		}
		if cfg.Antiban.ReadMaxIntervalMS > cfg.Antiban.MinIntervalMS {
			t.Fatalf("%s 的读间隔不应大于写间隔", p)
		}
		// 出口 IP 探测必须是 https 的“裸 IP 回显”服务；api.ipify.org 在部分国内线路
		// 和代理节点上会直接断连（实测断连），换回它会让 auto 模式静默失效。
		if !strings.HasPrefix(cfg.Antiban.ExitIPProbeURL, "https://") {
			t.Fatalf("%s 的 exit_ip_probe_url 必须是 https 地址，实际 %q", p, cfg.Antiban.ExitIPProbeURL)
		}
		if strings.Contains(cfg.Antiban.ExitIPProbeURL, "api.ipify.org") {
			t.Fatalf("%s 的 exit_ip_probe_url 使用了 api.ipify.org：国内线路/代理常直接断连，建议 https://ifconfig.me/ip", p)
		}
		return
	}
	t.Skip("未找到 config.yaml / config.example.yaml")
}

func TestToAntibanOptionsMapsUnits(t *testing.T) {
	a := withAntibanDefaults(AntibanConfig{
		MinIntervalMS:      250,
		MaxIntervalMS:      750,
		BreakerCooldownSec: 30,
		DeviceTemplates: []AntibanDeviceTemplate{
			{Model: "Xiaomi 13", UserAgent: "ua-13"},
		},
	})
	opts := a.ToAntibanOptions("fallback-ua")

	if opts.MinInterval != 250*time.Millisecond {
		t.Fatalf("min interval 换算错误: %s", opts.MinInterval)
	}
	if opts.MaxInterval != 750*time.Millisecond {
		t.Fatalf("max interval 换算错误: %s", opts.MaxInterval)
	}
	if opts.BreakerCooldown != 30*time.Second {
		t.Fatalf("冷却时间换算错误: %s", opts.BreakerCooldown)
	}
	if opts.FallbackUserAgent != "fallback-ua" {
		t.Fatalf("fallback UA 未传递: %q", opts.FallbackUserAgent)
	}
	if len(opts.DeviceTemplates) != 1 || opts.DeviceTemplates[0].Model != "Xiaomi 13" {
		t.Fatalf("设备模板未转换: %+v", opts.DeviceTemplates)
	}
}
