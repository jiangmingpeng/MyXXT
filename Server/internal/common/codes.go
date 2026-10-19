package common

// 业务错误码（与前端 Web/src/constants.ts 中的 SIGN_ERR_* 保持一致）。
const (
	CodeOK   = 0
	CodeFail = 1

	// CodeQRCodeExpired 表示二维码 enc 已失效，前端应提示用户重新扫码/粘贴。
	CodeQRCodeExpired = 2001
	// CodeCredentialExpired 表示该同学的学习通登录态失效，需要重新登录。
	CodeCredentialExpired = 2002
	// CodeSignEnded 表示签到活动已结束。
	CodeSignEnded = 2003
)
