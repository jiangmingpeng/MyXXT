package service

import (
	"log"
	"strings"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"xbt2/server/internal/antiban"
	"xbt2/server/internal/model"
)

// AccountProfileStore 把账号的"设备 + 出口"画像持久化到 PostgreSQL。
//
// 反风控引擎只依赖 antiban.Store 这个窄接口，数据库实现放在 service 层，
// 这样 antiban 包可以完全脱离 gorm 单独测试。
type AccountProfileStore struct {
	db *gorm.DB

	mu    sync.RWMutex
	cache map[string]antiban.Profile
}

// NewAccountProfileStore 构造带内存缓存的画像存储。
func NewAccountProfileStore(db *gorm.DB) *AccountProfileStore {
	return &AccountProfileStore{
		db:    db,
		cache: make(map[string]antiban.Profile),
	}
}

// Load 实现 antiban.Store。命中后写入内存缓存，避免每次请求都查库。
func (s *AccountProfileStore) Load(mobile string) (antiban.Profile, bool) {
	mobile = strings.TrimSpace(mobile)
	if mobile == "" || s == nil || s.db == nil {
		return antiban.Profile{}, false
	}

	s.mu.RLock()
	if p, ok := s.cache[mobile]; ok {
		s.mu.RUnlock()
		return p, true
	}
	s.mu.RUnlock()

	var row model.AccountProfile
	if err := s.db.Where("mobile = ?", mobile).First(&row).Error; err != nil {
		// 没有画像属于正常情况（首次登录），不打印错误。
		return antiban.Profile{}, false
	}

	p := antiban.Profile{
		Mobile:      row.Mobile,
		UserAgent:   row.UserAgent,
		DeviceCode:  row.DeviceCode,
		DeviceModel: row.DeviceModel,
		ProxyURL:    row.ProxyURL,
		ExitIP:      row.ExitIP,
	}
	s.mu.Lock()
	s.cache[mobile] = p
	s.mu.Unlock()
	return p, true
}

// Save 实现 antiban.Store：按 mobile 做 upsert，字段为空时保留旧值。
func (s *AccountProfileStore) Save(p antiban.Profile) error {
	mobile := strings.TrimSpace(p.Mobile)
	if mobile == "" || s == nil || s.db == nil {
		return nil
	}

	// 先更新内存缓存：即使数据库暂时不可用，本次进程内指纹也保持稳定。
	s.mu.Lock()
	merged := s.cache[mobile]
	merged.Mobile = mobile
	if p.UserAgent != "" {
		merged.UserAgent = p.UserAgent
	}
	if p.DeviceCode != "" {
		merged.DeviceCode = p.DeviceCode
	}
	if p.DeviceModel != "" {
		merged.DeviceModel = p.DeviceModel
	}
	if p.ProxyURL != "" {
		merged.ProxyURL = p.ProxyURL
	}
	if p.ExitIP != "" {
		merged.ExitIP = p.ExitIP
	}
	s.cache[mobile] = merged
	s.mu.Unlock()

	row := model.AccountProfile{
		Mobile:      mobile,
		DeviceModel: merged.DeviceModel,
		UserAgent:   merged.UserAgent,
		DeviceCode:  merged.DeviceCode,
		ProxyURL:    merged.ProxyURL,
		ExitIP:      merged.ExitIP,
	}
	err := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "mobile"}},
		DoUpdates: clause.AssignmentColumns([]string{"device_model", "user_agent", "device_code", "proxy_url", "exit_ip", "updated_at"}),
	}).Create(&row).Error
	if err != nil {
		log.Printf("[antiban] save account profile failed mobile=%s err=%v", maskMobile(mobile), err)
		return err
	}
	return nil
}

// BindUID 在登录成功后把画像与真实 uid 关联，便于后台排查。
func (s *AccountProfileStore) BindUID(mobile string, uid int64) error {
	mobile = strings.TrimSpace(mobile)
	if mobile == "" || uid <= 0 || s == nil || s.db == nil {
		return nil
	}
	return s.db.Model(&model.AccountProfile{}).
		Where("mobile = ?", mobile).
		Update("uid", uid).Error
}

func maskMobile(mobile string) string {
	if len(mobile) < 7 {
		return "***"
	}
	return mobile[:3] + "****" + mobile[len(mobile)-4:]
}
