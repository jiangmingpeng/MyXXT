package service

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"xbt2/server/internal/antiban"
	"xbt2/server/internal/common"
	"xbt2/server/internal/model"
	"xbt2/server/internal/xxt"
)

type SignService struct {
	db       *gorm.DB
	xxt      *xxt.Client
	cc       *CredentialCrypto
	encCache *CredentialCache
	guard    antiban.Guard
}

// NewSignService 构造签到服务。
// guard 为 nil 时不做任何节流/熔断判断（仅用于单元测试）。
func NewSignService(db *gorm.DB, xxtClient *xxt.Client, cc *CredentialCrypto, guard antiban.Guard) *SignService {
	return &SignService{db: db, xxt: xxtClient, cc: cc, encCache: NewCredentialCache(), guard: guard}
}

// riskBlockedResult 把本地熔断状态翻译成前端可展示的结果。
func riskBlockedResult(uid int64, remaining time.Duration) SignExecuteResult {
	sec := int(remaining.Seconds())
	if sec < 1 {
		sec = 1
	}
	return SignExecuteResult{
		UserID:  uid,
		Success: false,
		Code:    common.CodeRiskBlocked,
		Message: fmt.Sprintf("该账号已触发风控保护，请在 %d 秒后再试（本次未向学习通发送请求）", sec),
	}
}

// precheckRisk 在真正发请求之前做一次熔断判断。
// 这一步的意义是：已经被学习通限流的账号，越重试越难恢复。
func (s *SignService) precheckRisk(uid int64, mobile string) *SignExecuteResult {
	if s.guard == nil {
		return nil
	}
	if st := s.guard.Status(mobile); st.Blocked {
		res := riskBlockedResult(uid, st.Remaining)
		return &res
	}
	return nil
}

type ExecuteSignRequest struct {
	ActivityID   int64
	TargetUID    int64
	SignType     int
	CourseID     int64
	ClassID      int64
	IfRefreshEWM bool
	Special      map[string]interface{}
}

type ExecutePhotoSignRequest struct {
	ActivityID   int64
	TargetUID    int64
	CourseID     int64
	ClassID      int64
	IfRefreshEWM bool
	ObjectID     string
	Filename     string
	ContentType  string
	Photo        []byte
}

type SignCheckItem struct {
	UserID           int64  `json:"user_id"`
	Signed           bool   `json:"signed"`
	RecordSource     int64  `json:"record_source"`
	RecordSourceName string `json:"record_source_name"`
	Message          string `json:"message"`
}

type SignExecuteResult struct {
	UserID           int64  `json:"user_id"`
	Success          bool   `json:"success"`
	AlreadySigned    bool   `json:"already_signed"`
	RecordSource     int64  `json:"record_source"`
	RecordSourceName string `json:"record_source_name"`
	Message          string `json:"message"`
	// Code 为业务错误码：0=成功，2001=二维码失效，2002=凭据失效，2003=签到已结束。
	Code int `json:"code"`
	// LocationDistanceMeters 是位置校验失败时学习通返回的“距签到圆心距离（米）”。
	// 学习通不下发圆心经纬度，这个距离是唯一精确标尺，
	// 前端据此做三边定位，反解出老师的真实圆心。为空表示本次返回没带距离。
	LocationDistanceMeters *float64 `json:"location_distance_meters,omitempty"`
}

// withLocationDistance 把 errorLocation2_<米> 里的距离挂到结果上，供前端校准圆心。
func withLocationDistance(res SignExecuteResult, raw string) SignExecuteResult {
	if d, ok := xxt.ParseLocationErrorDistance(raw); ok {
		res.LocationDistanceMeters = &d
	}
	return res
}

func (s *SignService) CheckSignStates(activityID int64, userIDs []int64) ([]SignCheckItem, error) {
	if activityID <= 0 {
		return nil, errors.New("invalid activity_id")
	}
	uniq := dedupeUIDs(userIDs)
	items := make([]SignCheckItem, 0, len(uniq))
	for _, uid := range uniq {
		items = append(items, s.resolveSignState(activityID, uid))
	}
	return items, nil
}

func (s *SignService) ExecuteOne(operatorUID int64, req ExecuteSignRequest) SignExecuteResult {
	state := s.resolveSignState(req.ActivityID, req.TargetUID)
	if state.Signed {
		return SignExecuteResult{
			UserID:           req.TargetUID,
			Success:          true,
			AlreadySigned:    true,
			RecordSource:     state.RecordSource,
			RecordSourceName: state.RecordSourceName,
			Message:          state.Message,
		}
	}

	var target model.User
	if err := s.db.Where("uid = ?", req.TargetUID).First(&target).Error; err != nil {
		return SignExecuteResult{UserID: req.TargetUID, Success: false, Message: "该同学未登录或账号不可用"}
	}
	password, err := s.cc.Decrypt(target.CredentialCipher)
	if err != nil {
		return SignExecuteResult{UserID: req.TargetUID, Success: false, Message: "该同学登录信息已过期，请先重新登录"}
	}
	// 本地熔断检查：账号已在冷却期内时直接返回，不再打扰学习通。
	if blocked := s.precheckRisk(req.TargetUID, target.Mobile); blocked != nil {
		return *blocked
	}

	fixed := xxt.FixedParams{
		ActiveID:     req.ActivityID,
		UID:          req.TargetUID,
		CourseID:     req.CourseID,
		ClassID:      req.ClassID,
		IfRefreshEWM: req.IfRefreshEWM,
	}
	// 二维码签到：优先使用本次请求携带的凭据（enc + c + 查询策略）。
	// 缓存只作为兜底：请求没带 enc 时复用上一次成功使用过的凭据。
	qrEnc, qrCode := "", ""
	qrDBStrategy, qrStrategyPara := "", ""
	if req.SignType == xxt.SignQRCode {
		qrEnc, _ = req.Special["enc"].(string)
		qrCode, _ = req.Special["c"].(string)
		qrDBStrategy, _ = req.Special["db_strategy"].(string)
		qrStrategyPara, _ = req.Special["strategy_para"].(string)
		qrEnc = strings.TrimSpace(qrEnc)
		qrCode = strings.TrimSpace(qrCode)
		qrDBStrategy = strings.TrimSpace(qrDBStrategy)
		qrStrategyPara = strings.TrimSpace(qrStrategyPara)

		if cached, ok := s.encCache.Get(req.ActivityID); ok && cached.Enc != "" {
			// 请求带了新 enc 时以请求为准，避免旧缓存覆盖刚扫到的新码。
			if qrEnc == "" {
				qrEnc, qrCode = cached.Enc, cached.C
			} else if qrEnc == cached.Enc && qrCode == "" {
				qrCode = cached.C
			}
			if qrDBStrategy == "" {
				qrDBStrategy = cached.DBStrategy
			}
			if qrStrategyPara == "" {
				qrStrategyPara = cached.StrategyPara
			}
		}
		if qrEnc == "" {
			return SignExecuteResult{
				UserID:  req.TargetUID,
				Success: false,
				Code:    common.CodeQRCodeExpired,
				Message: "二维码参数缺失，请重新扫码或粘贴",
			}
		}

		fixed.DBStrategy = qrDBStrategy
		fixed.StrategyPara = qrStrategyPara
		// 刷新二维码链接一定带 c（sign_code），静态码没有。
		// 即使活动详情没标记 if_refresh_ewm，也按刷新流程走预签到 rcode，避免漏参数。
		if qrCode != "" {
			fixed.IfRefreshEWM = true
		}

		// 刷新二维码的 enc 时效很短（约 5~10s），必须尽量压缩单次签到耗时。
		// 验证码使用独立临时 cookie jar，与预签到互不依赖，所以先在后台单飞启动，
		// 再同步做预签到；不再用 WaitGroup 等"两个都完成"，也不会在预取失败时
		// 让 xxt.Sign 把整套验证码重解一遍。
		stageStarted := time.Now()
		s.xxt.StartCaptchaPrefetch(target.Mobile, fixed)
		preSignErr := s.xxt.PreSign(target.Mobile, password, fixed, qrCode, qrEnc)
		preSignCost := time.Since(stageStarted)

		if preSignErr != nil {
			return SignExecuteResult{UserID: req.TargetUID, Success: false, Message: "预签到失败，请重试"}
		}
		if req.Special == nil {
			req.Special = map[string]interface{}{}
		}
		req.Special["enc"] = qrEnc
		if qrCode != "" {
			req.Special["c"] = qrCode
		}
		// 只取"已经算好"的验证码；还在途就留空，交给 xxt.Sign 内部等同一个任务。
		prefetched := false
		if validate, ok := s.xxt.TakePrefetchedCaptcha(target.Mobile, fixed); ok && validate != "" {
			req.Special["validate"] = validate
			prefetched = true
		}
		signStarted := time.Now()
		defer func() {
			log.Printf("[timing] qr sign activity=%d uid=%d preSign=%s prefetched=%t sign=%s total=%s",
				fixed.ActiveID, fixed.UID,
				preSignCost.Round(time.Millisecond), prefetched,
				time.Since(signStarted).Round(time.Millisecond),
				time.Since(stageStarted).Round(time.Millisecond))
		}()
	}

	result, err := s.xxt.Sign(target.Mobile, password, fixed, req.SignType, req.Special)
	if err != nil {
		// 节流/熔断是在发请求阶段抛出的，属于"本地保护"，单独给错误码。
		var blocked *antiban.BlockedError
		if errors.As(err, &blocked) {
			return riskBlockedResult(req.TargetUID, blocked.Remaining)
		}
		if req.SignType == xxt.SignQRCode && xxt.IsQRCodeExpired(err.Error()) {
			s.encCache.DeleteIfEnc(req.ActivityID, qrEnc)
			return SignExecuteResult{
				UserID:  req.TargetUID,
				Success: false,
				Code:    common.CodeQRCodeExpired,
				Message: "二维码已过期，请重新扫码或粘贴",
			}
		}
		return withLocationDistance(
			SignExecuteResult{UserID: req.TargetUID, Success: false, Message: s.toUserSignMessage(err.Error())},
			err.Error(),
		)
	}
	result = strings.TrimSpace(result)
	if result != "success" {
		// 学习通已经明确提示账号异常/操作频繁时，继续重试只会让限制更久。
		if antiban.IsBlocked(result) {
			return SignExecuteResult{
				UserID:  req.TargetUID,
				Success: false,
				Code:    common.CodeRiskBlocked,
				Message: "该账号已被学习通限制，请暂停使用并稍后再试",
			}
		}
		if strings.Contains(result, "您已签到过了") {
			rec := model.SignRecord{UserUID: req.TargetUID, ActivityID: req.ActivityID, SourceUID: -1, SignTimeMS: time.Now().UnixMilli()}
			_ = s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_uid"}, {Name: "activity_id"}}, DoNothing: true}).Create(&rec).Error
			return SignExecuteResult{
				UserID:           req.TargetUID,
				Success:          true,
				AlreadySigned:    true,
				RecordSource:     -1,
				RecordSourceName: "学习通",
				Message:          "该同学已在学习通签到",
			}
		}
		if req.SignType == xxt.SignQRCode && xxt.IsQRCodeExpired(result) {
			s.encCache.DeleteIfEnc(req.ActivityID, qrEnc)
			return SignExecuteResult{
				UserID:  req.TargetUID,
				Success: false,
				Code:    common.CodeQRCodeExpired,
				Message: "二维码已过期，请重新扫码或粘贴",
			}
		}
		return withLocationDistance(
			SignExecuteResult{UserID: req.TargetUID, Success: false, Message: s.toUserSignMessage(result)},
			result,
		)
	}

	// 仅在签到成功后写入缓存：保证缓存里的 enc 一定被学习通接受过。
	if req.SignType == xxt.SignQRCode && qrEnc != "" {
		s.encCache.Set(req.ActivityID, QRCredential{
			Enc:          qrEnc,
			C:            qrCode,
			DBStrategy:   qrDBStrategy,
			StrategyPara: qrStrategyPara,
		})
	}

	rec := model.SignRecord{
		UserUID:    req.TargetUID,
		ActivityID: req.ActivityID,
		SourceUID:  operatorUID,
		SignTimeMS: time.Now().UnixMilli(),
	}
	if err := s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_uid"}, {Name: "activity_id"}}, DoNothing: true}).Create(&rec).Error; err != nil {
		return SignExecuteResult{UserID: req.TargetUID, Success: false, Message: "保存签到结果失败，请重试"}
	}

	sourceName := s.getSourceName(operatorUID)
	if strings.TrimSpace(sourceName) == "" {
		sourceName = "未知用户"
	}
	return SignExecuteResult{
		UserID:           req.TargetUID,
		Success:          true,
		AlreadySigned:    false,
		RecordSource:     operatorUID,
		RecordSourceName: sourceName,
		Message:          "签到成功",
	}
}

func (s *SignService) ExecutePhoto(operatorUID int64, req ExecutePhotoSignRequest) SignExecuteResult {
	if req.ActivityID <= 0 || req.CourseID <= 0 || req.ClassID <= 0 {
		return SignExecuteResult{UserID: req.TargetUID, Success: false, Message: "签到参数不完整"}
	}
	if req.TargetUID <= 0 {
		req.TargetUID = operatorUID
	}
	state := s.resolveSignState(req.ActivityID, req.TargetUID)
	if state.Signed {
		return SignExecuteResult{
			UserID:           req.TargetUID,
			Success:          true,
			AlreadySigned:    true,
			RecordSource:     state.RecordSource,
			RecordSourceName: state.RecordSourceName,
			Message:          state.Message,
		}
	}

	objectID := strings.TrimSpace(req.ObjectID)
	if objectID == "" {
		if len(req.Photo) == 0 {
			return SignExecuteResult{UserID: req.TargetUID, Success: false, Message: "请上传照片或传入 object_id"}
		}
		var target model.User
		if err := s.db.Where("uid = ?", req.TargetUID).First(&target).Error; err != nil {
			return SignExecuteResult{UserID: req.TargetUID, Success: false, Message: "该同学未登录或账号不可用"}
		}
		password, err := s.cc.Decrypt(target.CredentialCipher)
		if err != nil {
			return SignExecuteResult{UserID: req.TargetUID, Success: false, Message: "该同学登录信息已过期，请先重新登录"}
		}
		objectID, err = s.xxt.UploadPanFile(target.Mobile, password, req.Filename, req.ContentType, req.Photo)
		if err != nil {
			return SignExecuteResult{UserID: req.TargetUID, Success: false, Message: "照片上传失败：" + err.Error()}
		}
	}

	return s.ExecuteOne(operatorUID, ExecuteSignRequest{
		ActivityID:   req.ActivityID,
		TargetUID:    req.TargetUID,
		SignType:     xxt.SignNormal,
		CourseID:     req.CourseID,
		ClassID:      req.ClassID,
		IfRefreshEWM: req.IfRefreshEWM,
		Special: map[string]interface{}{
			"object_id": objectID,
		},
	})
}

func (s *SignService) toUserSignMessage(raw string) string {
	msg := strings.TrimSpace(raw)
	if msg == "" {
		return "签到失败，请稍后重试"
	}

	lower := strings.ToLower(msg)
	// 学习通的定位校验失败（errorLocation3 / errorLocation_xxx）需要单独翻译：
	// 这类返回说明定位参数已被接受，只是坐标不在老师设置的签到范围内。
	if hint := xxt.ExplainLocationError(msg); hint != "" {
		return hint
	}
	switch {
	case msg == "validate" || strings.Contains(lower, "validate"):
		return "签到校验未通过，请重试"
	case strings.Contains(msg, "验证码识别失败") || strings.Contains(lower, "captcha"):
		return "验证码校验失败，请重试"
	case strings.Contains(msg, "缺少二维码 enc 参数"):
		return "二维码参数缺失，请刷新活动后重试"
	case strings.Contains(msg, "缺少 sign_code 参数"):
		return "签到码缺失，请输入后重试"
	case strings.Contains(msg, "请求过于频繁"):
		return "操作太频繁，请稍后再试"
	case strings.Contains(msg, "活动已结束"):
		return "该签到已结束"
	case strings.Contains(msg, "签到成功"):
		return "签到成功"
	case strings.Contains(msg, "您已签到过了"):
		return "该同学已在学习通签到"
	default:
		return msg
	}
}

func (s *SignService) resolveSignState(activityID, uid int64) SignCheckItem {
	state := SignCheckItem{UserID: uid, Signed: false, RecordSource: 0, RecordSourceName: "", Message: "未签到"}
	if activityID <= 0 || uid <= 0 {
		return state
	}

	var rec model.SignRecord
	if err := s.db.Where("user_uid = ? AND activity_id = ?", uid, activityID).Take(&rec).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return state
		}
		state.Message = "查询失败"
		return state
	}

	state.Signed = true
	state.RecordSource = rec.SourceUID
	if rec.SourceUID == -1 {
		state.RecordSourceName = "学习通"
		state.Message = "该同学已在学习通签到"
		return state
	}
	if rec.SourceUID == uid {
		state.RecordSourceName = s.getSourceName(uid)
		if state.RecordSourceName == "" {
			state.RecordSourceName = "本人"
		}
		state.Message = "该同学已本人签到"
		return state
	}
	state.RecordSourceName = s.getSourceName(rec.SourceUID)
	if state.RecordSourceName == "" {
		state.RecordSourceName = "未知用户"
	}
	state.Message = fmt.Sprintf("该同学已被%s代签", state.RecordSourceName)
	return state
}

func (s *SignService) getSourceName(sourceUID int64) string {
	if sourceUID <= 0 {
		return ""
	}
	var user model.User
	if err := s.db.Where("uid = ?", sourceUID).Take(&user).Error; err != nil {
		return ""
	}
	return strings.TrimSpace(user.Name)
}

func dedupeUIDs(userIDs []int64) []int64 {
	set := make(map[int64]struct{}, len(userIDs))
	out := make([]int64, 0, len(userIDs))
	for _, uid := range userIDs {
		if uid <= 0 {
			continue
		}
		if _, ok := set[uid]; ok {
			continue
		}
		set[uid] = struct{}{}
		out = append(out, uid)
	}
	return out
}
