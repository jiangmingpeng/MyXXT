package xxt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func extractBetween(s, left, right string) string {
	i := strings.Index(s, left)
	if i < 0 {
		return ""
	}
	s = s[i+len(left):]
	j := strings.Index(s, right)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(s[:j])
}

func strVal(v interface{}) string {
	s, ok := v.(string)
	if ok {
		return s
	}
	s2, ok := v.(fmt.Stringer)
	if ok {
		return s2.String()
	}
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func int64FromAny(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	default:
		return 0
	}
}

// floatFromAny 兼容 JSON 数字 / 数字字符串两种经纬度写法。
func floatFromAny(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func firstNonNil(values ...interface{}) interface{} {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func findActiveList(payload interface{}) []map[string]interface{} {
	switch p := payload.(type) {
	case map[string]interface{}:
		if arr, ok := p["activeList"].([]interface{}); ok {
			return normalizeActiveList(arr)
		}
		for _, v := range p {
			if out := findActiveList(v); len(out) > 0 {
				return out
			}
		}
	case []interface{}:
		for _, v := range p {
			if out := findActiveList(v); len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

func normalizeActiveList(arr []interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out
}

func findBestActivityArray(payload interface{}) []map[string]interface{} {
	var best []map[string]interface{}
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for _, child := range x {
				walk(child)
			}
		case []interface{}:
			candidate := normalizeActiveList(x)
			if len(candidate) > 0 {
				score := scoreActivityArray(candidate)
				bestScore := scoreActivityArray(best)
				if score > bestScore {
					best = candidate
				}
			}
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(payload)
	return best
}

func scoreActivityArray(arr []map[string]interface{}) int {
	if len(arr) == 0 {
		return 0
	}
	score := 0
	for _, m := range arr {
		if int64FromAny(firstNonNil(m["id"], m["activeId"], m["active_id"])) > 0 {
			score += 2
		}
		if strVal(firstNonNil(m["nameOne"], m["name"], m["activeName"], m["title"])) != "" {
			score += 2
		}
		if int64FromAny(firstNonNil(m["activeType"], m["type"], m["atype"])) > 0 {
			score += 1
		}
	}
	return score
}

func deepFindFirst(v interface{}, keys ...string) interface{} {
	for _, k := range keys {
		if val, ok := deepFindKey(v, k); ok {
			return val
		}
	}
	return nil
}

func deepFindKey(v interface{}, target string) (interface{}, bool) {
	switch x := v.(type) {
	case map[string]interface{}:
		if val, ok := x[target]; ok {
			return val, true
		}
		for _, child := range x {
			if val, ok := deepFindKey(child, target); ok {
				return val, true
			}
		}
	case []interface{}:
		for _, child := range x {
			if val, ok := deepFindKey(child, target); ok {
				return val, true
			}
		}
	}
	return nil, false
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// JSONLeafPairs 把任意 JSON 值摊平成 "路径=值" 列表（数组只看前 3 个元素）。
// 用于诊断日志：不用猜字段名，直接把返回里所有疑似定位字段打印出来。
func JSONLeafPairs(v interface{}, max int) []string {
	out := make([]string, 0, 32)
	collectLeafPairs(v, "", &out, max)
	sort.Strings(out)
	return out
}

func collectLeafPairs(v interface{}, prefix string, out *[]string, max int) {
	if len(*out) >= max {
		return
	}
	switch x := v.(type) {
	case map[string]interface{}:
		for k, child := range x {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			collectLeafPairs(child, p, out, max)
			if len(*out) >= max {
				return
			}
		}
	case []interface{}:
		for i, child := range x {
			if i >= 3 {
				break
			}
			collectLeafPairs(child, fmt.Sprintf("%s[%d]", prefix, i), out, max)
			if len(*out) >= max {
				return
			}
		}
	default:
		if prefix == "" {
			return
		}
		*out = append(*out, fmt.Sprintf("%s=%v", prefix, x))
	}
}

// TopLevelKeys 返回 JSON 对象的顶层键（排序后），用于日志判断返回结构。
func TopLevelKeys(v interface{}) []string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func parseTimeMillis(v interface{}) int64 {
	if v == nil {
		return 0
	}
	if m, ok := v.(map[string]interface{}); ok {
		return int64FromAny(m["time"])
	}
	return int64FromAny(v)
}

func boolFromAny(v interface{}) bool {
	switch b := v.(type) {
	case bool:
		return b
	case float64:
		return b != 0
	case int:
		return b != 0
	case int64:
		return b != 0
	case string:
		return b == "1" || strings.EqualFold(b, "true")
	default:
		return false
	}
}

// qrCodeExpiredKeywords 是学习通 stuSignajax 在二维码 enc 失效时可能返回的文案。
// 已实测：enc 无效时返回 "签到失败，请重新扫描。"（validate 是否携带都一样）。
var qrCodeExpiredKeywords = []string{
	"请重新扫描",
	"重新扫描二维码",
	"二维码已过期",
	"二维码过期",
	"二维码失效",
	"二维码已失效",
	"二维码已刷新",
	"无效二维码",
	"无效的二维码",
	"请刷新二维码",
	"请重新扫码",
	"enc已失效",
	"enc 已失效",
}

// IsQRCodeExpired 判断 stuSignajax 的返回（纯文本或 JSON）是否表示二维码 enc 已失效。
// 注意：活动结束（“活动已结束”）属于 CodeSignEnded，不在此列。
func IsQRCodeExpired(body string) bool {
	msg := strings.TrimSpace(body)
	if msg == "" {
		return false
	}
	// 学习通部分接口返回 JSON，优先结构化解析，避免误判。
	if strings.HasPrefix(msg, "{") {
		msg = stuSignMessageFromJSON(msg)
	}
	lower := strings.ToLower(msg)
	for _, kw := range qrCodeExpiredKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// locationErrorRe 匹配学习通位置校验失败的返回。实测格式：
//
//	errorLocation3        学习通没拿到可用定位（没有距离后缀）
//	errorLocation2_1137.0 距离老师设置的签到圆心 1137 米
//
// 第二个捕获组是距离（单位米），可能带小数。
var locationErrorRe = regexp.MustCompile(`(?i)errorLocation[_]?(\d+)(?:_(\d+(?:\.\d+)?))?`)

// ParseLocationErrorDistance 从学习通位置校验失败的返回里提取“距签到圆心多少米”。
//
// 只有带 “_距离” 后缀的才算有效距离：
//
//	errorLocation2_1137.0 -> 1137 米
//	errorLocation3        -> 不是距离（3 是错误码）
//
// 这个距离是唯一可信的圆心标尺（学习通不下发圆心经纬度），前端的三边定位
// 校准就是靠反复探测这个距离来反解真实圆心。
func ParseLocationErrorDistance(raw string) (float64, bool) {
	msg := strings.TrimSpace(raw)
	if msg == "" {
		return 0, false
	}
	if strings.HasPrefix(msg, "{") {
		msg = stuSignMessageFromJSON(msg)
	}
	m := locationErrorRe.FindStringSubmatch(msg)
	if len(m) < 3 || m[2] == "" {
		return 0, false
	}
	d, err := strconv.ParseFloat(m[2], 64)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// ExplainLocationError 把 stusignajax 的位置校验错误翻译成可操作的中文提示。
// 返回空字符串表示该返回不是位置校验错误。
func ExplainLocationError(raw string) string {
	msg := strings.TrimSpace(raw)
	if msg == "" {
		return ""
	}
	if strings.HasPrefix(msg, "{") {
		msg = stuSignMessageFromJSON(msg)
	}
	m := locationErrorRe.FindStringSubmatch(msg)
	if len(m) < 2 {
		return ""
	}
	// errorLocation2_920.0 这种带距离后缀的，直接把距离告诉用户，比“超出范围”更好定位问题。
	if len(m) > 2 && m[2] != "" {
		if d, err := strconv.ParseFloat(m[2], 64); err == nil && d > 0 {
			return fmt.Sprintf("位置校验未通过：距签到点约 %d 米（老师设置的签到范围更小），请靠近签到点重新选点", int64(d+0.5))
		}
	}
	switch m[1] {
	case "0", "3":
		// errorLocation3：学习通没拿到可用的定位（坐标缺失或格式不合法）。
		return "学习通未获取到有效定位，请重新在地图上选点后再试"
	default:
		return "位置校验未通过：提交的坐标不在老师设置的签到范围内，请靠近签到点重新选点"
	}
}

// stuSignMessageFromJSON 从 JSON 返回中提取可读文案；解析失败时原样返回。
func stuSignMessageFromJSON(body string) string {
	var payload struct {
		Msg      string `json:"msg"`
		Message  string `json:"message"`
		ErrorMsg string `json:"errorMsg"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return body
	}
	for _, s := range []string{payload.Msg, payload.Message, payload.ErrorMsg, payload.Error} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return body
}
