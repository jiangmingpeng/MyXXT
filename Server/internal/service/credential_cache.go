package service

import (
	"strings"
	"sync"
)

// QRCredential 是某个签到活动可复用的二维码凭据。
// Enc 用于 stuSignajax；C（sign_code）在 if_refresh_ewm 活动的预签到 rcode 中需要。
// DBStrategy / StrategyPara 是刷新码链接里的查询策略，换码后仍需沿用。
type QRCredential struct {
	Enc          string
	C            string
	DBStrategy   string
	StrategyPara string
}

// CredentialCache 以 activeId 为 key 缓存二维码凭据，避免二维码刷新后
// 每个代签目标都要重新扫码/上传。内存版：单实例够用，进程重启即清空。
type CredentialCache struct {
	mu sync.RWMutex
	m  map[int64]QRCredential
}

func NewCredentialCache() *CredentialCache {
	return &CredentialCache{m: make(map[int64]QRCredential)}
}

func (c *CredentialCache) Get(activeID int64) (QRCredential, bool) {
	if c == nil || activeID <= 0 {
		return QRCredential{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	cred, ok := c.m[activeID]
	return cred, ok
}

func (c *CredentialCache) Set(activeID int64, cred QRCredential) {
	if c == nil || activeID <= 0 {
		return
	}
	cred.Enc = strings.TrimSpace(cred.Enc)
	cred.C = strings.TrimSpace(cred.C)
	cred.DBStrategy = strings.TrimSpace(cred.DBStrategy)
	cred.StrategyPara = strings.TrimSpace(cred.StrategyPara)
	if cred.Enc == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[activeID] = cred
}

func (c *CredentialCache) Delete(activeID int64) {
	if c == nil || activeID <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, activeID)
}

// DeleteIfEnc 仅在缓存中的 enc 与给定值一致时删除，避免并发失败请求
// 误删其他请求刚刚缓存的有效凭据。
func (c *CredentialCache) DeleteIfEnc(activeID int64, enc string) {
	if c == nil || activeID <= 0 {
		return
	}
	enc = strings.TrimSpace(enc)
	if enc == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.m[activeID]; ok && strings.TrimSpace(cur.Enc) == enc {
		delete(c.m, activeID)
	}
}
