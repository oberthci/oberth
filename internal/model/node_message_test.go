package model

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeNodeMessage(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"plain message", "plain message"},
		{"\x1b[31mred\x1b[0m\x00\n\r\t\u0085\u202e", "red"},
		{"\x1b]0;hidden title\aok", "ok"},
		{"\x1b]8;;https://example.test\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"\u009b31mred\u009b0m\u009dhidden\u009cvisible", "redvisible"},
		{"\x1bPprivate control string\x1b\\ok\x1b(B", "ok"},
		{"prefix\x1b[123;", "prefix"},
		{strings.Repeat("界", 600), strings.Repeat("界", 512)},
	} {
		if got := SanitizeNodeMessage(tt.input); got != tt.want || !utf8.ValidString(got) {
			t.Errorf("sanitize %q = %q, want %q", tt.input, got, tt.want)
		}
	}
}
