/**
 * 后端业务错误码，需与 Server/internal/common/codes.go 保持一致。
 */
export const API_CODE_OK = 0;
export const API_CODE_FAIL = 1;

/** 二维码 enc 已失效，需要重新扫码或粘贴 */
export const SIGN_ERR_QR_EXPIRED = 2001;
/** 该同学的学习通登录态失效，需要重新登录 */
export const SIGN_ERR_CREDENTIAL_EXPIRED = 2002;
/** 签到活动已结束 */
export const SIGN_ERR_SIGN_ENDED = 2003;
