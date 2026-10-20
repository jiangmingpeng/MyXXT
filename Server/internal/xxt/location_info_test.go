package xxt

import (
	"encoding/json"
	"strings"
	"testing"
)

// 解析 getPPTActiveInfo 的典型返回：data 里带签到圆心、半径、地址和 ifopenAddress。
func TestApplyLocationFieldsFromActiveInfo(t *testing.T) {
	var payload map[string]interface{}
	raw := `{
		"result": 1,
		"data": {
			"activeId": 123456,
			"otherId": 2,
			"ifopenAddress": 1,
			"locationLatitude": 28.286512,
			"locationLongitude": 109.725431,
			"locationRange": 500,
			"locationAddress": "湖南省吉首市人民南路120号"
		}
	}`
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	var detail SignDetail
	applyLocationFields(&detail, payload)

	if !detail.IfOpenAddress {
		t.Fatal("IfOpenAddress = false, want true")
	}
	if detail.LocationLatitude != 28.286512 {
		t.Fatalf("LocationLatitude = %v, want 28.286512", detail.LocationLatitude)
	}
	if detail.LocationLongitude != 109.725431 {
		t.Fatalf("LocationLongitude = %v, want 109.725431", detail.LocationLongitude)
	}
	if detail.LocationRange != 500 {
		t.Fatalf("LocationRange = %v, want 500", detail.LocationRange)
	}
	if detail.LocationAddress != "湖南省吉首市人民南路120号" {
		t.Fatalf("LocationAddress = %q", detail.LocationAddress)
	}
}

// 兼容旧字段名（activeLatitude/activeLongitude/activeRange）与字符串坐标。
func TestApplyLocationFieldsLegacyKeys(t *testing.T) {
	var payload map[string]interface{}
	raw := `{"activeLatitude":"30.1","activeLongitude":"120.2","activeRange":"1000"}`
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	var detail SignDetail
	applyLocationFields(&detail, payload)

	if detail.LocationLatitude != 30.1 || detail.LocationLongitude != 120.2 {
		t.Fatalf("coords = (%v, %v), want (30.1, 120.2)", detail.LocationLatitude, detail.LocationLongitude)
	}
	if detail.LocationRange != 1000 {
		t.Fatalf("LocationRange = %v, want 1000", detail.LocationRange)
	}
	// 没有显式 ifopenAddress，但拿到了圆心 → 视为启用位置校验。
	if !detail.IfOpenAddress {
		t.Fatal("IfOpenAddress = false, want true")
	}
}

// 实测 2026-10-20 的 getPPTActiveInfo 返回：经纬度全是空串/0，只有
// locationRange + locationText（格式化地址 + POI 名）可用。
// 这种返回必须解析出半径和地址，同时不能把 (0,0) 当成合法圆心。
func TestApplyLocationFieldsRealActiveInfoDump(t *testing.T) {
	raw := `{
		"result": 1,
		"data": {
			"activeId": 6000177345246,
			"otherId": 4,
			"ifopenAddress": 1,
			"ifgetrange": null,
			"latitude": 0,
			"longitude": 0,
			"locationLatitude": "",
			"locationLongitude": "",
			"locationLatitude_gd": "",
			"locationLongitude_gd": "",
			"locationRange": 1000,
			"locationText": "湖南省湘西土家族苗族自治州吉首市人民南路120号 吉首大学-图书馆"
		}
	}`
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	var detail SignDetail
	applyLocationFields(&detail, payload)

	if detail.LocationRange != 1000 {
		t.Fatalf("LocationRange = %v, want 1000", detail.LocationRange)
	}
	if detail.LocationAddress != "湖南省湘西土家族苗族自治州吉首市人民南路120号 吉首大学-图书馆" {
		t.Fatalf("LocationAddress = %q, want locationText", detail.LocationAddress)
	}
	if ValidLatLng(detail.LocationLatitude, detail.LocationLongitude) {
		t.Fatalf("coords = (%v, %v), want invalid (server strips center)",
			detail.LocationLatitude, detail.LocationLongitude)
	}
	// 有半径就说明该活动启用了位置校验，前端要据此画圈/提示。
	if !detail.IfOpenAddress {
		t.Fatal("IfOpenAddress = false, want true (range present)")
	}
}

// 空的 locationLatitude 不能挡住后面成对出现的有效坐标。
func TestLocationPairFromAnySkipsEmptyFirstCandidate(t *testing.T) {
	var payload map[string]interface{}
	raw := `{"data":{"locationLatitude":"","locationLongitude":"","activeLatitude":"28.1","activeLongitude":"109.7"}}`
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	lat, lng, ok := locationPairFromAny(payload)
	if !ok {
		t.Fatal("locationPairFromAny = false, want true")
	}
	if lat != 28.1 || lng != 109.7 {
		t.Fatalf("coords = (%v, %v), want (28.1, 109.7)", lat, lng)
	}
}

// 没有定位字段的活动不能被误判成“启用了位置校验”。
func TestApplyLocationFieldsWithoutLocation(t *testing.T) {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(`{"data":{"otherId":2,"ifopenAddress":0}}`), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	var detail SignDetail
	applyLocationFields(&detail, payload)

	if detail.IfOpenAddress {
		t.Fatal("IfOpenAddress = true, want false")
	}
	if detail.LocationRange != 0 || detail.LocationLatitude != 0 || detail.LocationLongitude != 0 {
		t.Fatalf("unexpected location fields: %+v", detail)
	}
}

// 0 坐标（学习通占位值）必须被丢弃，否则会算出天安门外的距离。
func TestApplyLocationFieldsIgnoresZeroCoords(t *testing.T) {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(`{"locationLatitude":0,"locationLongitude":0,"locationRange":500}`), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	var detail SignDetail
	applyLocationFields(&detail, payload)

	if detail.LocationLatitude != 0 || detail.LocationLongitude != 0 {
		t.Fatalf("coords = (%v, %v), want (0, 0)", detail.LocationLatitude, detail.LocationLongitude)
	}
	// 半径仍然有效，因此依旧是位置签到。
	if !detail.IfOpenAddress || detail.LocationRange != 500 {
		t.Fatalf("detail = %+v, want range 500 and IfOpenAddress true", detail)
	}
}

// 嵌套对象形态的 location 不能被当成地址字符串。
func TestApplyLocationFieldsSkipsObjectAddress(t *testing.T) {
	var payload map[string]interface{}
	raw := `{"locationLatitude":28.1,"locationLongitude":109.7,"location":{"lat":28.1,"lng":109.7}}`
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	var detail SignDetail
	applyLocationFields(&detail, payload)

	if detail.LocationAddress != "" {
		t.Fatalf("LocationAddress = %q, want empty", detail.LocationAddress)
	}
}

func TestExplainLocationError(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"errorLocation3", "学习通未获取到有效定位，请重新在地图上选点后再试"},
		{`{"msg":"errorLocation3"}`, "学习通未获取到有效定位，请重新在地图上选点后再试"},
		// 实测格式：errorLocation2_1137.0，后缀是距离（米）。
		{"errorLocation2_1137.0", "位置校验未通过：距签到点约 1137 米（老师设置的签到范围更小），请靠近签到点重新选点"},
		{"errorLocation2_920.4", "位置校验未通过：距签到点约 920 米（老师设置的签到范围更小），请靠近签到点重新选点"},
		{`{"msg":"errorLocation2_1137.0"}`, "位置校验未通过：距签到点约 1137 米（老师设置的签到范围更小），请靠近签到点重新选点"},
		{"errorLocation_500", "位置校验未通过：提交的坐标不在老师设置的签到范围内，请靠近签到点重新选点"},
		{"errorLocation1000", "位置校验未通过：提交的坐标不在老师设置的签到范围内，请靠近签到点重新选点"},
		{"success", ""},
		{"签到失败，请重新扫描。", ""},
	}
	for _, c := range cases {
		if got := ExplainLocationError(c.raw); got != c.want {
			t.Errorf("ExplainLocationError(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// 诊断日志依赖摊平后的路径，字段名对不上时能直接从日志里看出来。
func TestJSONLeafPairs(t *testing.T) {
	var payload map[string]interface{}
	raw := `{"result":1,"data":{"active":{"locationLatitude":"28.1","locationRange":500},"list":[{"a":1},{"a":2}]}}`
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	pairs := JSONLeafPairs(payload, 50)
	joined := strings.Join(pairs, ",")
	for _, want := range []string{"data.active.locationLatitude=28.1", "data.active.locationRange=500", "data.list[1].a=2", "result=1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("JSONLeafPairs missing %q in %s", want, joined)
		}
	}
	if got := TopLevelKeys(payload); len(got) != 2 || got[0] != "data" || got[1] != "result" {
		t.Errorf("TopLevelKeys = %v, want [data result]", got)
	}
}

// taskactivelist 里自带定位时应当能直接解析出来（省掉一次 getPPTActiveInfo）。
func TestActiveLocationDetailFromList(t *testing.T) {
	var item map[string]interface{}
	raw := `{"id":6000177327632,"nameOne":"签到","activeType":2,"otherId":2,
		"locationLatitude":28.2867,"locationLongitude":109.725565,"locationRange":300,"locationAddress":"图书馆"}`
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	loc := SignDetail{}
	applyLocationFields(&loc, item)
	loc.LocationFetched = ValidLatLng(loc.LocationLatitude, loc.LocationLongitude)
	a := Active{
		ActiveID:          6000177327632,
		Name:              "签到",
		IfOpenAddress:     loc.IfOpenAddress,
		LocationLatitude:  loc.LocationLatitude,
		LocationLongitude: loc.LocationLongitude,
		LocationRange:     loc.LocationRange,
		LocationAddress:   loc.LocationAddress,
		LocationFetched:   loc.LocationFetched,
	}
	got := a.LocationDetail()
	if !got.LocationFetched || got.LocationRange != 300 || got.LocationAddress != "图书馆" {
		t.Fatalf("LocationDetail = %+v, want fetched with range 300 and address 图书馆", got)
	}
	if got.LocationLatitude != 28.2867 || got.LocationLongitude != 109.725565 {
		t.Fatalf("coords = (%v, %v)", got.LocationLatitude, got.LocationLongitude)
	}
}

func TestFloatFromAny(t *testing.T) {
	cases := []struct {
		in   interface{}
		want float64
		ok   bool
	}{
		{float64(28.29), 28.29, true},
		{"109.72", 109.72, true},
		{"  ", 0, false},
		{nil, 0, false},
		{"abc", 0, false},
		{int64(500), 500, true},
	}
	for _, c := range cases {
		got, ok := floatFromAny(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("floatFromAny(%#v) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
