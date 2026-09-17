package cline

import "testing"

// ParseExpiry 兼容上游给的多种过期时间形态（毫秒数字符串/数字/RFC3339 时间串）。
func TestParseExpiry(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
	}{
		{"float64 毫秒", float64(1735689600000), 1735689600000},
		{"int64 毫秒", int64(1735689600000), 1735689600000},
		{"int 毫秒", 1735689600000, 1735689600000},
		{"RFC3339", "2025-01-01T00:00:00Z", 1735689600000},
		{"RFC3339 带纳秒", "2025-01-01T00:00:00.5Z", 1735689600500},
		{"无法解析的字符串", "not-a-time", 0},
		{"nil", nil, 0},
		{"未知类型", []int{1}, 0},
	}
	for _, tc := range cases {
		if got := ParseExpiry(tc.in); got != tc.want {
			t.Fatalf("%s: ParseExpiry(%v) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}