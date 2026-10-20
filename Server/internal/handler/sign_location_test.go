package handler

import (
	"testing"

	"xbt2/server/internal/model"
	"xbt2/server/internal/xxt"
)

// 缓存行 → SignDetail：位置校准字段必须完整带出，否则列表接口会返回 0 坐标。
func TestDetailFromActivityCacheKeepsLocation(t *testing.T) {
	cache := model.SignActivity{
		ActivityID:        1001,
		StartTime:         1700000000000,
		EndTime:           1700000600000,
		SignType:          xxt.SignQRCode,
		IfRefreshEWM:      true,
		IfPhoto:           false,
		IfOpenAddress:     true,
		LocationLatitude:  28.286512,
		LocationLongitude: 109.725431,
		LocationRange:     500,
		LocationAddress:   "图书馆",
		LocationFetched:   true,
	}

	detail := detailFromActivityCache(cache)

	if detail.StartTime != cache.StartTime || detail.EndTime != cache.EndTime {
		t.Fatalf("time = (%d, %d), want (%d, %d)", detail.StartTime, detail.EndTime, cache.StartTime, cache.EndTime)
	}
	if detail.SignType != xxt.SignQRCode || !detail.IfRefreshEWM {
		t.Fatalf("sign type/flags = (%d, %v), want (%d, true)", detail.SignType, detail.IfRefreshEWM, xxt.SignQRCode)
	}
	if !detail.IfOpenAddress || detail.LocationRange != 500 {
		t.Fatalf("location flags = (%v, %d), want (true, 500)", detail.IfOpenAddress, detail.LocationRange)
	}
	if detail.LocationLatitude != 28.286512 || detail.LocationLongitude != 109.725431 {
		t.Fatalf("coords = (%v, %v)", detail.LocationLatitude, detail.LocationLongitude)
	}
	if detail.LocationAddress != "图书馆" || !detail.LocationFetched {
		t.Fatalf("address/fetched = (%q, %v)", detail.LocationAddress, detail.LocationFetched)
	}
}

// 补拉远端位置时只能覆盖位置字段，缓存里的时间/类型必须保留（否则活动会“变短”或换类型）。
func TestMergeActivityLocationKeepsCachedMeta(t *testing.T) {
	cached := xxt.SignDetail{
		StartTime:    1700000000000,
		EndTime:      1700000600000,
		SignType:     xxt.SignQRCode,
		IfRefreshEWM: true,
	}
	remote := xxt.SignDetail{
		StartTime:         1, // 远端时间不参与合并
		EndTime:           2,
		SignType:          xxt.SignLocation,
		IfOpenAddress:     true,
		LocationLatitude:  28.2,
		LocationLongitude: 109.7,
		LocationRange:     1000,
		LocationAddress:   "教学楼",
		LocationFetched:   true,
	}

	merged := mergeActivityLocation(cached, remote)

	if merged.StartTime != cached.StartTime || merged.EndTime != cached.EndTime {
		t.Fatalf("time = (%d, %d), want cached values", merged.StartTime, merged.EndTime)
	}
	if merged.SignType != cached.SignType {
		t.Fatalf("sign type = %d, want %d", merged.SignType, cached.SignType)
	}
	if !merged.LocationFetched || merged.LocationRange != 1000 || merged.LocationAddress != "教学楼" {
		t.Fatalf("location not merged: %+v", merged)
	}
}

// 空的位置信息不能把缓存里已有的圆心/半径冲掉（活动列表项经常缺这些字段）。
func TestMergeActivityLocationIgnoresEmptyRemote(t *testing.T) {
	cached := xxt.SignDetail{
		StartTime:         1700000000000,
		EndTime:           1700000600000,
		SignType:          xxt.SignQRCode,
		IfOpenAddress:     true,
		LocationLatitude:  28.2867,
		LocationLongitude: 109.725565,
		LocationRange:     500,
		LocationAddress:   "图书馆",
		LocationFetched:   true,
	}

	merged := mergeActivityLocation(cached, xxt.SignDetail{})

	if merged.LocationLatitude != cached.LocationLatitude || merged.LocationLongitude != cached.LocationLongitude {
		t.Fatalf("coords = (%v, %v), want cached values", merged.LocationLatitude, merged.LocationLongitude)
	}
	if merged.LocationRange != 500 || merged.LocationAddress != "图书馆" || !merged.IfOpenAddress || !merged.LocationFetched {
		t.Fatalf("merged = %+v, want cached location preserved", merged)
	}
}

// 老版本把“接口请求成功”写成 location_fetched=true，但坐标是 0，
// 必须以坐标为准，否则这类行永远不会再补拉。
func TestActivityLocationComplete(t *testing.T) {
	stale := model.SignActivity{LocationFetched: true, LocationLatitude: 0, LocationLongitude: 0}
	if activityLocationComplete(stale) {
		t.Fatal("activityLocationComplete(stale) = true, want false")
	}
	ok := model.SignActivity{LocationFetched: true, LocationLatitude: 28.28, LocationLongitude: 109.72}
	if !activityLocationComplete(ok) {
		t.Fatal("activityLocationComplete(ok) = false, want true")
	}
	notFetched := model.SignActivity{LocationLatitude: 28.28, LocationLongitude: 109.72}
	if activityLocationComplete(notFetched) {
		t.Fatal("activityLocationComplete(notFetched) = true, want false")
	}
}

func TestSignTypeNeedsLocation(t *testing.T) {
	cases := []struct {
		signType int
		want     bool
	}{
		{xxt.SignQRCode, true},
		{xxt.SignLocation, true},
		{xxt.SignNormal, false},
		{xxt.SignGesture, false},
		{xxt.SignCode, false},
	}
	for _, c := range cases {
		if got := signTypeNeedsLocation(c.signType); got != c.want {
			t.Errorf("signTypeNeedsLocation(%d) = %v, want %v", c.signType, got, c.want)
		}
	}
}
