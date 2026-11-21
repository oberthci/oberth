package client

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestPlainHTTPRefusalNamesTheTransport(t *testing.T) {
	err := validateAddress("http://localhost:8080")
	var problem *publicerr.Error
	if !errors.As(err, &problem) || !strings.Contains(problem.Reason(), "HTTP") || publicerr.CheckMessage(problem.Reason(), 80) != nil {
		t.Fatal("HTTP refusal does not explain transport", err)
	}
}

func TestCAMismatchExplainsHowToBypassImplicitProfile(t *testing.T) {
	f := newAuthorityBinding(t)
	_, err := Resolve(Options{CAFile: f.override}, f.base, func(string) string { return "" })
	var problem *publicerr.Error
	if !errors.As(err, &problem) {
		t.Fatal("mismatch was not refused", err)
	}
	for _, flag := range []string{"--url", "--ca-file", "--token-file"} {
		if !strings.Contains(problem.Remedy(), flag) {
			t.Fatal("implicit-profile exit lacks explicit credential option", problem.Remedy())
		}
	}
	if publicerr.CheckMessage(problem.Remedy(), 120) != nil {
		t.Fatal("remedy exceeds message limit")
	}
	token := filepath.Join(f.base, "standalone-token")
	if err := WritePrivate(token, []byte("standalone-bearer")); err != nil {
		t.Fatal(err)
	}
	options, err := Resolve(Options{URL: f.address, CAFile: f.override, TokenFile: token}, f.base, func(string) string { return "" })
	if err != nil || options.TokenFile != token || options.ClientCert != "" || options.ClientKey != "" {
		t.Fatal("documented exit inherited profile identity", options, err)
	}
}
