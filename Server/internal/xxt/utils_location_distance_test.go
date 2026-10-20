package xxt

import "testing"

// 位置校验失败返回里带出的距离是前端三边定位校准的唯一标尺，
// 解析必须只认“_距离”后缀，且不能被错误码混淆。
func TestParseLocationErrorDistance(t *testing.T) {
	cases := []struct {
		body   string
		want   float64
		wantOK bool
	}{
		{"errorLocation2_1137.0", 1137, true},
		{"errorLocation2_901.0", 901, true},
		{"errorLocation2_0.8", 0.8, true},
		{"errorLocation1_25", 25, true},
		{`{"msg":"errorLocation2_1322.5"}`, 1322.5, true},
		{"  errorLocation5_77.5  ", 77.5, true},
		// 没有距离后缀：数字是错误码，不是米数
		{"errorLocation3", 0, false},
		{"errorLocation_500", 0, false},
		// 非位置校验返回
		{"success", 0, false},
		{"该同学已在学习通签到", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseLocationErrorDistance(c.body)
		if ok != c.wantOK {
			t.Fatalf("ParseLocationErrorDistance(%q) ok = %v, want %v", c.body, ok, c.wantOK)
		}
		if ok && got != c.want {
			t.Fatalf("ParseLocationErrorDistance(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}
