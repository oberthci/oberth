package gitoid

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "valid SHA-1", value: "a94a8fe5ccb19ba61c4c0873d391e987982fbbd3", want: true},
		{name: "valid SHA-256", value: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", want: true},
		{name: "all zeros SHA-1", value: strings.Repeat("0", 40), want: true},
		{name: "all f SHA-256", value: strings.Repeat("f", 64), want: true},
		{name: "uppercase hex rejected", value: "A94A8FE5CCB19BA61C4C0873D391E987982FBBD3", want: false},
		{name: "mixed case rejected", value: "a94a8fe5ccb19ba61c4c0873d391e987982fBBd3", want: false},
		{name: "too short", value: "a94a8fe5ccb19ba61c4c0873d391e987982fbb", want: false},
		{name: "too long for SHA-1", value: "a94a8fe5ccb19ba61c4c0873d391e987982fbbd3a", want: false},
		{name: "41 chars", value: strings.Repeat("a", 41), want: false},
		{name: "63 chars", value: strings.Repeat("a", 63), want: false},
		{name: "65 chars", value: strings.Repeat("a", 65), want: false},
		{name: "empty", value: "", want: false},
		{name: "non-hex characters", value: "g94a8fe5ccb19ba61c4c0873d391e987982fbbd3", want: false},
		{name: "spaces", value: " a94a8fe5ccb19ba61c4c0873d391e987982fbbd", want: false},
		{name: "with newline", value: "a94a8fe5ccb19ba61c4c0873d391e987982fbbd\n", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Valid(tc.value); got != tc.want {
				t.Fatalf("Valid(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestValidTrimmed(t *testing.T) {
	sha1Lower := "a94a8fe5ccb19ba61c4c0873d391e987982fbbd3"
	sha1Upper := "A94A8FE5CCB19BA61C4C0873D391E987982FBBD3"
	sha256Lower := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "lowercase SHA-1", value: sha1Lower, want: true},
		{name: "uppercase SHA-1 accepted", value: sha1Upper, want: true},
		{name: "mixed case accepted", value: "A94a8fe5ccb19ba61c4c0873d391e987982fBBd3", want: true},
		{name: "leading whitespace", value: "  " + sha1Lower, want: true},
		{name: "trailing whitespace", value: sha1Lower + "  ", want: true},
		{name: "surrounding whitespace", value: " " + sha1Lower + " ", want: true},
		{name: "leading tab", value: "\t" + sha1Lower, want: true},
		{name: "SHA-256 lowercase", value: sha256Lower, want: true},
		{name: "too short after trim", value: " abc ", want: false},
		{name: "empty", value: "", want: false},
		{name: "only whitespace", value: "   ", want: false},
		{name: "non-hex after trim", value: " g" + strings.Repeat("0", 39), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidTrimmed(tc.value); got != tc.want {
				t.Fatalf("ValidTrimmed(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestSame(t *testing.T) {
	sha := "a94a8fe5ccb19ba61c4c0873d391e987982fbbd3"
	shaUpper := "A94A8FE5CCB19BA61C4C0873D391E987982FBBD3"

	for _, tc := range []struct {
		name   string
		first  string
		second string
		want   bool
	}{
		{name: "identical", first: sha, second: sha, want: true},
		{name: "case insensitive", first: sha, second: shaUpper, want: true},
		{name: "with leading space", first: " " + sha, second: sha, want: true},
		{name: "with trailing space", first: sha, second: sha + " ", want: true},
		{name: "both whitespace", first: " " + sha + " ", second: "\t" + shaUpper + "\n", want: true},
		{name: "different values", first: sha, second: strings.Repeat("0", 40), want: false},
		{name: "empty both", first: "", second: "", want: true},
		{name: "empty vs non-empty", first: "", second: sha, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Same(tc.first, tc.second); got != tc.want {
				t.Fatalf("Same(%q, %q) = %v, want %v", tc.first, tc.second, got, tc.want)
			}
		})
	}
}

func TestPathWithin(t *testing.T) {
	for _, tc := range []struct {
		name string
		root string
		path string
		want bool
	}{
		{name: "child path", root: "/home/user", path: "/home/user/file.txt", want: true},
		{name: "nested child", root: "/home/user", path: "/home/user/a/b/c", want: true},
		{name: "exact root", root: "/home/user", path: "/home/user", want: true},
		{name: "parent escape", root: "/home/user", path: "/home/other", want: false},
		{name: "dot-dot escape", root: "/home/user", path: "/home/user/../other", want: false},
		{name: "sibling", root: "/home/user", path: "/home/user2", want: false},
		{name: "root of fs", root: "/", path: "/anything", want: true},
		{name: "relative paths", root: "a/b", path: "a/b/c", want: true},
		{name: "relative escape", root: "a/b", path: "a/c", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PathWithin(tc.root, tc.path); got != tc.want {
				t.Fatalf("PathWithin(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
			}
		})
	}
}

func TestStrictPathWithin(t *testing.T) {
	for _, tc := range []struct {
		name string
		root string
		path string
		want bool
	}{
		{name: "absolute clean child", root: "/home/user", path: "/home/user/file.txt", want: true},
		{name: "exact root", root: "/home/user", path: "/home/user", want: true},
		{name: "relative path rejected", root: "/home/user", path: "home/user/file.txt", want: false},
		{name: "unclean path rejected", root: "/home/user", path: "/home/user/./file.txt", want: false},
		{name: "dot-dot in path rejected", root: "/home/user", path: "/home/user/../user/file.txt", want: false},
		{name: "trailing slash rejected", root: "/home/user", path: "/home/user/dir/", want: false},
		{name: "parent escape rejected", root: "/home/user", path: "/home/other", want: false},
		{name: "absolute outside root", root: "/home/user", path: "/etc/passwd", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StrictPathWithin(tc.root, tc.path); got != tc.want {
				t.Fatalf("StrictPathWithin(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
			}
		})
	}
}
