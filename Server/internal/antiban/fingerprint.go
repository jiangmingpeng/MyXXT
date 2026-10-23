package antiban

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// deviceModelRe 从学习通 App 的 UA 里抽取机型：
// "... (schild:xxxx) (device:Redmi K20 Pro) Language/zh_CN ..."
var deviceModelRe = regexp.MustCompile(`\(device:([^)]*)\)`)

// DeviceModelFromUA 从 UA 里解析机型号，解析不到时返回 "Android"。
func DeviceModelFromUA(ua string) string {
	if m := deviceModelRe.FindStringSubmatch(ua); len(m) == 2 {
		if v := strings.TrimSpace(m[1]); v != "" {
			return v
		}
	}
	return "Android"
}

// StableDeviceCode 派生账号级稳定的设备标识（32 位小写 hex）。
//
// 真机的 deviceCode 由设备特征算出，生命周期内不变；
// 之前的实现每次登录都随机生成，等于每次登录都换一台手机，
// 本身就是一条很明显的行为异常。这里用 mobile+机型做确定性派生：
// 同一账号永远得到同一个值，不同账号之间也不相同。
func StableDeviceCode(mobile, model string) string {
	seed := strings.TrimSpace(mobile) + "|" + strings.TrimSpace(model) + "|chaoxing-device-v1"
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}

// BuildUserAgent 按学习通 App 的真实格式拼出一条 UA。
//
// 格式参考官方 Android 客户端：
//
//	Dalvik/2.1.0 (Linux; U; Android {android}; {model} Build/{build}) (schild:{schild})
//	(device:{model}) Language/zh_CN com.chaoxing.mobile/ChaoXingStudy_{ver}_android_phone_{build}
//	_264 (@Kalimdor)_{tail}
//
// 其中 schild 是设备特征哈希、tail 是安装级随机串，两者都应与 deviceCode 一样"账号级稳定"。
func BuildUserAgent(model, android, build, appVersion string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "Redmi K20 Pro"
	}
	if android == "" {
		android = "13"
	}
	if build == "" {
		build = "TKQ1.221114.001"
	}
	if appVersion == "" {
		appVersion = "3_6.4.8"
	}
	schild := StableDeviceCode(model, "schild")
	tail := StableDeviceCode(model, "kalimdor")
	return fmt.Sprintf(
		"Dalvik/2.1.0 (Linux; U; Android %s; %s Build/%s) (schild:%s) (device:%s) Language/zh_CN com.chaoxing.mobile/ChaoXingStudy_%s_android_phone_10834_264 (@Kalimdor)_%s",
		android, model, build, truncateHex(schild, 32), model, appVersion, truncateHex(tail, 32),
	)
}

func truncateHex(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
