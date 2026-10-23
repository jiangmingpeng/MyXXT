package antiban

import (
	"net/http"
	"strconv"
	"strings"
)

// rateLimitHints 是学习通/网关在限流或风控时常见的返回文案。
var rateLimitHints = []string{
	"请求过于频繁",
	"操作过于频繁",
	"操作太频繁",
	"访问过于频繁",
	"太频繁",
	"系统繁忙",
	"请稍后再试",
	"请稍后重试",
	"稍后再试",
	"请求过多",
}

// blockedHints 是明确的"账号被限制/存在风险"文案。
var blockedHints = []string{
	"账号异常",
	"账号存在风险",
	"存在风险",
	"已被限制",
	"已被冻结",
	"禁止访问",
	"登录失败次数过多",
	"拒绝访问",
}

// locationHints 是定位校验类业务错误，不算风控。
var locationHints = []string{
	"errorlocation",
	"定位",
	"位置校验",
	"不在签到范围",
}

// captchaHints 表示学习通要求人机校验。
var captchaHints = []string{
	"validate",
	"验证码",
	"人机校验",
}

// ClassifyResponse 把一次学习通返回归类成风控信号。
// body 传入原始响应体，status 传入 HTTP 状态码（0 表示未知）。
func ClassifyResponse(status int, body string) Signal {
	switch {
	case status == http.StatusTooManyRequests:
		return SignalRateLimited
	case status == http.StatusForbidden:
		return SignalBlocked
	case status >= 500:
		return SignalTransient
	}

	lower := strings.ToLower(body)
	if lower == "" {
		return SignalNone
	}

	for _, h := range blockedHints {
		if strings.Contains(body, h) {
			return SignalBlocked
		}
	}
	for _, h := range rateLimitHints {
		if strings.Contains(body, h) {
			return SignalRateLimited
		}
	}
	for _, h := range locationHints {
		if strings.Contains(lower, h) {
			return SignalLocationRejected
		}
	}
	for _, h := range captchaHints {
		if strings.Contains(lower, h) {
			return SignalCaptcha
		}
	}
	return SignalNone
}

// ClassifySignBody 把 stuSignajax 的返回归类成签到结果信号。
// 与 ClassifyResponse 的差别：这里能区分"签到成功"。
func ClassifySignBody(status int, body string) Signal {
	trimmed := strings.TrimSpace(body)
	lower := strings.ToLower(trimmed)

	// 学习通成功返回 "success" 或包含 success 的 JSON。
	if lower == "success" || strings.Contains(lower, `"success":true`) || strings.Contains(lower, `"result":1`) {
		return SignalSuccess
	}
	if sig := ClassifyResponse(status, body); sig != SignalNone {
		return sig
	}
	return SignalNone
}

// IsBlocked 判断错误里是不是"账号被限制"。
func IsBlocked(body string) bool {
	lower := strings.ToLower(body)
	for _, h := range blockedHints {
		if strings.Contains(body, h) || strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

// ParseRetryAfter 解析 Retry-After 头（秒），解析失败返回 0。
func ParseRetryAfter(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
