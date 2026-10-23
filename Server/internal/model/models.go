package model

import "time"

type User struct {
	ID               uint      `gorm:"primaryKey" json:"-"`
	UID              int64     `gorm:"unique;not null" json:"uid"`
	Mobile           string    `gorm:"size:32;unique;not null" json:"mobile"`
	Name             string    `gorm:"size:128;not null" json:"name"`
	Avatar           string    `gorm:"size:1024" json:"avatar"`
	CredentialCipher string    `gorm:"type:text;not null" json:"-"`
	Permission       int       `gorm:"not null;default:1" json:"permission"`
	LastLoginAt      time.Time `json:"-"`
	CreatedAt        time.Time `json:"-"`
	UpdatedAt        time.Time `json:"-"`
}

type Whitelist struct {
	ID         uint      `gorm:"primaryKey" json:"-"`
	Mobile     string    `gorm:"size:32;unique;not null" json:"Mobile"`
	Permission int       `gorm:"not null;default:1" json:"Permission"`
	CreatedAt  time.Time `json:"-"`
	UpdatedAt  time.Time `json:"-"`
}

type Course struct {
	ID        uint      `gorm:"primaryKey" json:"-"`
	CourseID  int64     `gorm:"not null;uniqueIndex:idx_course_class" json:"course_id"`
	ClassID   int64     `gorm:"not null;uniqueIndex:idx_course_class" json:"class_id"`
	Name      string    `gorm:"size:255;not null" json:"name"`
	Teacher   string    `gorm:"size:255" json:"teacher"`
	Icon      string    `gorm:"size:1024" json:"icon"`
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

type UserCourse struct {
	ID         uint      `gorm:"primaryKey" json:"-"`
	UserUID    int64     `gorm:"not null;uniqueIndex:idx_user_course_class" json:"-"`
	CourseID   int64     `gorm:"not null;uniqueIndex:idx_user_course_class" json:"course_id"`
	ClassID    int64     `gorm:"not null;uniqueIndex:idx_user_course_class" json:"class_id"`
	IsSelected bool      `gorm:"not null;default:false" json:"is_selected"`
	CreatedAt  time.Time `json:"-"`
	UpdatedAt  time.Time `json:"-"`
}

type SignActivity struct {
	ID           uint  `gorm:"primaryKey" json:"-"`
	ActivityID   int64 `gorm:"unique;not null" json:"activity_id"`
	StartTime    int64 `gorm:"not null" json:"start_time"`
	EndTime      int64 `gorm:"not null" json:"end_time"`
	SignType     int   `gorm:"not null" json:"sign_type"`
	IfRefreshEWM bool  `gorm:"not null;default:false" json:"if_refresh_ewm"`
	IfPhoto      bool  `gorm:"not null;default:false" json:"if_photo"`
	// 位置校验：老师在活动里设置的签到圆心、半径与地址。
	IfOpenAddress     bool    `gorm:"not null;default:false" json:"if_open_address"`
	LocationLatitude  float64 `gorm:"not null;default:0" json:"location_latitude"`
	LocationLongitude float64 `gorm:"not null;default:0" json:"location_longitude"`
	LocationRange     int64   `gorm:"not null;default:0" json:"location_range"`
	LocationAddress   string  `gorm:"size:255" json:"location_address"`
	// LocationFetched 标记是否已向学习通查过位置信息（老缓存升级后需要补拉一次）。
	LocationFetched bool      `gorm:"not null;default:false" json:"-"`
	CreatedAt       time.Time `json:"-"`
	UpdatedAt       time.Time `json:"-"`
}

type SignRecord struct {
	ID         uint      `gorm:"primaryKey" json:"-"`
	UserUID    int64     `gorm:"not null;uniqueIndex:idx_user_activity" json:"user_uid"`
	ActivityID int64     `gorm:"not null;uniqueIndex:idx_user_activity" json:"activity_id"`
	SourceUID  int64     `gorm:"not null" json:"source_uid"`
	SignTimeMS int64     `gorm:"not null" json:"sign_time_ms"`
	CreatedAt  time.Time `json:"-"`
	UpdatedAt  time.Time `json:"-"`
}

// AccountProfile 持久化账号的"设备 + 出口"画像。
//
// 反风控的核心前提是"同一账号永远表现为同一台手机、同一个出口"：
// 把指纹落库后，即使服务重启、会话过期、重新登录，
// 学习通看到的仍然是同一台设备，而不是每次登录都换一台新手机。
type AccountProfile struct {
	ID     uint   `gorm:"primaryKey" json:"-"`
	Mobile string `gorm:"size:32;unique;not null" json:"-"`
	UID    int64  `gorm:"index" json:"-"`
	// DeviceModel 是机型，例如 "Redmi K20 Pro"。
	DeviceModel string `gorm:"size:64" json:"-"`
	// UserAgent 是该账号专属的完整 UA（含 schild / @Kalimdor 段）。
	UserAgent string `gorm:"size:1024" json:"-"`
	// DeviceCode 是账号级稳定的设备标识。
	DeviceCode string `gorm:"size:64" json:"-"`
	// ProxyURL 是该账号固定使用的出口代理，空表示直连。
	ProxyURL string `gorm:"size:512" json:"-"`
	// ExitIP 是代理实际出口 IP，供 clientip 参数使用。
	ExitIP    string    `gorm:"size:64" json:"-"`
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

func (AccountProfile) TableName() string {
	return "account_profiles"
}
