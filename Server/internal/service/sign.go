package service

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"xbt2/server/internal/common"
	"xbt2/server/internal/model"
	"xbt2/server/internal/xxt"
)

type SignService struct {
	db       *gorm.DB
	xxt      *xxt.Client
	cc       *CredentialCrypto
	encCache *CredentialCache
}

func NewSignService(db *gorm.DB, xxtClient *xxt.Client, cc *CredentialCrypto) *SignService {
	return &SignService{db: db, xxt: xxtClient, cc: cc, encCache: NewCredentialCache()}
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
		// 预签到使用目标账号会话，验证码使用独立临时 cookie jar，两者互不依赖，可以安全并行。
		// 验证码预抓失败时留空，xxt.Sign 内部会同步抓取兜底（只影响速度，不影响正确性）。
		var (
			preSignErr error
			validate   string
			captchaErr error
		)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			preSignErr = s.xxt.PreSign(target.Mobile, password, fixed, qrCode, qrEnc)
		}()
		go func() {
			defer wg.Done()
			validate, captchaErr = s.xxt.FetchCaptchaValidate(fixed)
		}()
		wg.Wait()

		if preSignErr != nil {
			return SignExecuteResult{UserID: req.TargetUID, Success: false, Message: "预签到失败，请重试"}
		}
		if captchaErr != nil {
			log.Printf("[captcha] prefetch failed: activity=%d uid=%d err=%v", fixed.ActiveID, fixed.UID, captchaErr)
			validate = ""
		}
		if req.Special == nil {
			req.Special = map[string]interface{}{}
		}
		req.Special["enc"] = qrEnc
		if qrCode != "" {
			req.Special["c"] = qrCode
		}
		if validate != "" {
			// 交给 xxt.Sign 复用，避免再串行抓一次验证码。
			req.Special["validate"] = validate
		}
	}

	result, err := s.xxt.Sign(target.Mobile, password, fixed, req.SignType, req.Special)
	if err != nil {
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
