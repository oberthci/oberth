package client

import (
	"errors"
	"strings"
	"testing"
)

func TestSSHEndpointLengthAndHostBoundaries(t *testing.T) {
	const prefix = "ssh://git@oberth.local:"
	for _, size := range []int{4096, 4097, 1 << 20} {
		endpoint := prefix + strings.Repeat("0", size-len(prefix)-2) + "22"
		_, err := ValidateSSHEndpoint(endpoint, "https://oberth.local")
		if (err == nil) != (size == 4096) {
			t.Fatalf("length %d: %v", size, err)
		}
	}
	for _, pair := range [][2]string{
		{"ssh://git@\u212Aube.local:22", "https://kube.local"},
		{"ssh://git@ho\u017Ft.local:22", "https://host.local"},
		{"ssh://git@kube.local:22", "https://\u212Aube.local"},
	} {
		if _, err := ValidateSSHEndpoint(pair[0], pair[1]); err == nil {
			t.Fatal("Unicode folding substituted another host", pair)
		}
	}
	if _, err := ValidateSSHEndpoint("ssh://git@OBERTH.local:22", "https://oberth.LOCAL"); err != nil {
		t.Fatal("ASCII host case rejected", err)
	}
}

func TestSSHEndpointParseFailureRetainsNoCredential(t *testing.T) {
	_, err := ValidateSSHEndpoint("ssh://git:private-marker@oberth.local:22 ", "https://oberth.local")
	if err == nil {
		t.Fatal("malformed endpoint accepted")
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if strings.Contains(cause.Error(), "private-marker") {
			t.Fatal("credential retained by endpoint refusal")
		}
	}
}
