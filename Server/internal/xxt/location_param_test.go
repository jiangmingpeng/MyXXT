package xxt

import (
	"encoding/json"
	"testing"
)

// 学习通二维码签到只认 {"result":1,"latitude":...,"longitude":...,"address":...}。
// 结构对齐 ChaoxingSignFaker 的 addLocationDataParameter：
//   - result 是整数 1（不是字符串 "1"，也不是缺失）；
//   - 没有 altitude 字段；
//   - location 参数不带 mockData（mockData 只出现在 locationResult 里）。
func TestBuildQRCodeLocationParamFormat(t *testing.T) {
	got, ok := buildQRCodeLocationParam(map[string]interface{}{
		"latitude":    30.123456,
		"longitude":   120.654321,
		"description": "图书馆",
		"accuracy":    12.5,
	})
	if !ok {
		t.Fatal("expected location param")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("unmarshal location: %v", err)
	}
	if payload["result"] != float64(1) {
		t.Fatalf("result = %#v, want number 1", payload["result"])
	}
	if payload["address"] != "图书馆" {
		t.Fatalf("address = %v, want 图书馆", payload["address"])
	}
	if _, exists := payload["altitude"]; exists {
		t.Fatalf("altitude should be absent, got %v", payload["altitude"])
	}
	if payload["latitude"] != 30.123456 {
		t.Fatalf("latitude = %v, want 30.123456", payload["latitude"])
	}
	if payload["longitude"] != 120.654321 {
		t.Fatalf("longitude = %v, want 120.654321", payload["longitude"])
	}
	if _, exists := payload["mockData"]; exists {
		t.Fatalf("mockData should be absent, got %v", payload["mockData"])
	}
}

// 字符串坐标要转成数字，且不应携带 accuracy（学习通的二维码定位结构里没有该字段）。
func TestBuildQRCodeLocationParamStringCoords(t *testing.T) {
	got, ok := buildQRCodeLocationParam(map[string]interface{}{
		"latitude":  "30.1",
		"longitude": "120.2",
	})
	if !ok {
		t.Fatal("expected location param")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("unmarshal location: %v", err)
	}
	if payload["latitude"] != 30.1 {
		t.Fatalf("latitude = %v, want 30.1", payload["latitude"])
	}
	if _, exists := payload["accuracy"]; exists {
		t.Fatalf("accuracy should be absent, got %v", payload["accuracy"])
	}
}

// 兼容 1.0 直接透传 special["location"] 的场景。
func TestBuildQRCodeLocationParamPassthrough(t *testing.T) {
	raw := `{"result":1,"address":"x","latitude":1,"longitude":2}`
	got, ok := buildQRCodeLocationParam(map[string]interface{}{"location": raw})
	if !ok || got != raw {
		t.Fatalf("passthrough = %q, %v; want %q, true", got, ok, raw)
	}
}

// locationResult 与 location 结构相同，但必须额外携带 mockData 字符串。
// mockData 是“JSON 字符串”而非对象，用于向学习通声明未使用模拟定位。
func TestBuildQRCodeLocationResultParam(t *testing.T) {
	got, ok := buildQRCodeLocationResultParam(map[string]interface{}{
		"latitude":    30.123456,
		"longitude":   120.654321,
		"description": "图书馆",
	})
	if !ok {
		t.Fatal("expected locationResult param")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("unmarshal locationResult: %v", err)
	}
	if payload["result"] != float64(1) {
		t.Fatalf("result = %#v, want number 1", payload["result"])
	}
	mockData, ok := payload["mockData"].(string)
	if !ok {
		t.Fatalf("mockData should be a JSON string, got %#v", payload["mockData"])
	}
	if mockData != `{"strategy":0,"probability":-1}` {
		t.Fatalf("mockData = %q, want {\"strategy\":0,\"probability\":-1}", mockData)
	}
	if _, exists := payload["altitude"]; exists {
		t.Fatalf("altitude should be absent, got %v", payload["altitude"])
	}
}

// 1.0 透传的 location 用作 locationResult 时，需要补上 mockData 字符串。
func TestBuildQRCodeLocationResultParamPassthrough(t *testing.T) {
	raw := `{"result":1,"address":"x","latitude":1,"longitude":2}`
	got, ok := buildQRCodeLocationResultParam(map[string]interface{}{"location": raw})
	if !ok {
		t.Fatal("expected locationResult param")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("unmarshal locationResult: %v", err)
	}
	if payload["address"] != "x" {
		t.Fatalf("address = %v, want x", payload["address"])
	}
	if payload["mockData"] != `{"strategy":0,"probability":-1}` {
		t.Fatalf("mockData = %#v, want the anti-mock JSON string", payload["mockData"])
	}
}
