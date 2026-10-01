package periapsis

import (
	"strings"
	"testing"
)

func TestValidateSecretStorePath(t *testing.T) {
	valid := []string{"ci/data/r2-upload", "secret/data/nested/deep.name", "kv_v2/data/a-b_c.d"}
	for _, path := range valid {
		if err := ValidateSecretStorePath(path); err != nil {
			t.Fatalf("ValidateSecretStorePath(%q) = %v", path, err)
		}
	}
	invalid := map[string]string{
		"":                       "1-512 bytes",
		"/ci/data/x":             "clean non-empty segments",
		"ci/data/x/":             "clean non-empty segments",
		"ci//data":               "clean non-empty segments",
		"ci/./data":              "clean non-empty segments",
		"ci/../data":             "clean non-empty segments",
		"auth/kubernetes/role":   "reserved",
		"sys/raw":                "reserved",
		"identity/entity":        "reserved",
		"cubbyhole/x":            "reserved",
		"ci/data/x y":            "only ASCII",
		"ci/data/x\n":            "only ASCII",
		strings.Repeat("a", 513): "1-512 bytes",
	}
	for path, want := range invalid {
		if err := ValidateSecretStorePath(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("ValidateSecretStorePath(%q) = %v, want %q", path, err, want)
		}
	}
}

func TestParseUpstreamSecretStorePath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		want     UpstreamSecretPath
		upstream bool
		wantErr  string
	}{
		{name: "org-scoped", path: "oberth/upstream/acme/cosign", want: UpstreamSecretPath{Org: "acme", Secret: "cosign"}, upstream: true},
		{name: "repo-scoped", path: "oberth/upstream/acme/payments/gh-token", want: UpstreamSecretPath{Org: "acme", Repo: "payments", Secret: "gh-token"}, upstream: true},
		{name: "case preserved", path: "oberth/upstream/Acme/Repo/Token", want: UpstreamSecretPath{Org: "Acme", Repo: "Repo", Secret: "Token"}, upstream: true},
		{name: "system namespace passthrough", path: "oberth/data/release/cosign"},
		{name: "foreign mount passthrough", path: "ci/data/r2-upload"},
		{name: "bare upstream root", path: "oberth/upstream", wantErr: "org-scoped"},
		{name: "missing secret segment", path: "oberth/upstream/acme", wantErr: "org-scoped"},
		{name: "too deep", path: "oberth/upstream/acme/repo/plan/secret", wantErr: "org-scoped"},
		{name: "reserved raw spelling", path: "oberth/data/upstream/acme/secret", wantErr: "reserved upstream subtree"},
		{name: "reserved raw root", path: "oberth/data/upstream", wantErr: "reserved upstream subtree"},
		{name: "generic syntax still applies", path: "oberth/upstream/acme/../secret", wantErr: "clean non-empty segments"},
		{name: "charset still applies", path: "oberth/upstream/acme/to ken", wantErr: "only ASCII"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, upstream, err := ParseUpstreamSecretStorePath(test.path)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || upstream != test.upstream || got != test.want {
				t.Fatalf("parse = %+v, %v, %v; want %+v, %v", got, upstream, err, test.want, test.upstream)
			}
		})
	}
}

func TestUpstreamSecretPathFetchPathAndDeclared(t *testing.T) {
	orgScoped := UpstreamSecretPath{Org: "acme", Secret: "cosign"}
	if got := orgScoped.FetchPath("oberth"); got != "oberth/data/upstream/acme/cosign" {
		t.Fatalf("org fetch path = %q", got)
	}
	if got := orgScoped.Declared(); got != "oberth/upstream/acme/cosign" {
		t.Fatalf("org declared = %q", got)
	}
	if got := orgScoped.Scope(); got != "org" {
		t.Fatalf("org scope = %q", got)
	}
	repoScoped := UpstreamSecretPath{Org: "acme", Repo: "payments", Secret: "gh"}
	if got := repoScoped.FetchPath("oberth-ci"); got != "oberth-ci/data/upstream/acme/payments/gh" {
		t.Fatalf("repo fetch path = %q", got)
	}
	if got := repoScoped.Declared(); got != "oberth/upstream/acme/payments/gh" {
		t.Fatalf("repo declared = %q", got)
	}
	if got := repoScoped.Scope(); got != "repo" {
		t.Fatalf("repo scope = %q", got)
	}
	if err := ValidateSecretStorePath(repoScoped.FetchPath("oberth")); err != nil {
		t.Fatalf("canonical path failed generic validation: %v", err)
	}
}

func TestUpstreamSecretPathAuthorize(t *testing.T) {
	repoScoped := UpstreamSecretPath{Org: "acme", Repo: "payments", Secret: "gh"}
	orgScoped := UpstreamSecretPath{Org: "acme", Secret: "cosign"}
	tests := []struct {
		name    string
		path    UpstreamSecretPath
		org     string
		repo    string
		wantErr string
	}{
		{name: "repo-scoped exact match", path: repoScoped, org: "acme", repo: "payments"},
		{name: "org-scoped any repo of the org", path: orgScoped, org: "acme", repo: "anything"},
		{name: "org mismatch", path: repoScoped, org: "other", repo: "payments", wantErr: `belongs to org "other"`},
		{name: "repo mismatch", path: repoScoped, org: "acme", repo: "intruder", wantErr: "scoped to repository acme/payments"},
		{name: "case-variant org refused", path: orgScoped, org: "Acme", repo: "payments", wantErr: "belongs to org"},
		{name: "case-variant repo refused", path: repoScoped, org: "acme", repo: "Payments", wantErr: "scoped to repository"},
		{name: "empty identity org", path: orgScoped, org: "", repo: "payments", wantErr: "defines no usable org"},
		{name: "host-shaped identity org fails closed", path: orgScoped, org: "git@github.com:22", repo: "payments", wantErr: "defines no usable org"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.path.Authorize(test.org, test.repo)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
