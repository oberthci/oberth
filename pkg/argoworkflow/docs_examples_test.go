package argoworkflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// Submit-mode admission checks these fixtures; it does not execute workloads.
func TestDocumentationExamplesAdmit(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("../../docs/examples/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("documentation examples missing: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			r, p, hooks := documentationRequest(t, file, raw)
			prepared, err := prepareDocumentation(t, r, p, hooks)
			if err != nil || prepared == nil {
				t.Fatalf("example not admitted: result=%v error=%v", prepared, err)
			}
			bad := strings.Replace(string(raw), "mode: 0755", "mode: 0700", 1)
			if p.Capability == nil {
				if bad == string(raw) {
					t.Fatal("example has no declared source to check")
				}
				t.Run("invalid_source", func(t *testing.T) {
					documentationRefuses(t, documentationDocument(r, []byte(bad)), p, hooks)
				})
			}
			assertDocumentationAuthority(t, r, p, hooks)
			if err := documentationShellParameters(raw); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func documentationRequest(t *testing.T, file string, raw []byte) (admissioncontract.PrepareRequest, Policy, admissioncontract.PrepareHooks) {
	t.Helper()
	wf, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	r := documentationDocument(requestFor(t, wf), raw)
	p := Policy{ServerNamespace: "oberth", ArgoNamespace: "controller"}
	hooks := identityHooks()
	switch filepath.Base(file) {
	case "cache.yaml":
		p.PVCClaims = map[string]bool{"build-cache": true}
	case "resource.yaml":
		capability, err := os.ReadFile("../../docs/examples/resource-capability.json")
		if err != nil {
			t.Fatal(err)
		}
		if err := p.LoadCapability(t.Context(), "ci", capability); err != nil {
			t.Fatal(err)
		}
		r.IdentityRequest.ResourceAccount = p.Capability.ServiceAccount
	case "release.yaml":
		r.SelectedFile = ".oberth/release.yaml"
		r.SourceReceipt.File = r.SelectedFile
		r.Ref, r.Trigger = "refs/tags/v0.1.0", "tag"
		p.Release = true
		hooks.LoginAudience = "secretstore"
		r.Grants = []admissioncontract.GrantEvidence{{Repository: r.Repository, File: r.SelectedFile,
			Step: "release-secret-reaches-step", Kind: "secret", Path: "release/signing-key",
			Revision: "1", State: admissioncontract.StateVerified}}

	}
	return r, p, hooks
}

func documentationDocument(r admissioncontract.PrepareRequest, raw []byte) admissioncontract.PrepareRequest {
	r.Document = raw
	r.SourceReceipt.DocumentDigest = sourcecontract.Digest(raw)
	return r
}

func prepareDocumentation(t *testing.T, r admissioncontract.PrepareRequest, p Policy, hooks admissioncontract.PrepareHooks) (*admissioncontract.PreparedAdmission, error) {
	t.Helper()
	hooks.BindSource = func(_ context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
		return boundEvidence(r, d), nil
	}
	return Prepare(t.Context(), r, p, hooks)
}

func documentationRefuses(t *testing.T, r admissioncontract.PrepareRequest, p Policy, hooks admissioncontract.PrepareHooks) {
	t.Helper()
	if _, err := prepareDocumentation(t, r, p, hooks); err == nil {
		t.Fatal("invalid documentation example admitted")
	}
}

func assertDocumentationAuthority(t *testing.T, r admissioncontract.PrepareRequest, p Policy, hooks admissioncontract.PrepareHooks) {
	t.Helper()
	if p.Capability != nil {
		t.Run("missing_capability", func(t *testing.T) {
			p.Capability = nil
			documentationRefuses(t, r, p, hooks)
		})
	}
	if len(p.PVCClaims) > 0 {
		t.Run("missing_claim", func(t *testing.T) {
			p.PVCClaims = nil
			documentationRefuses(t, r, p, hooks)
		})
	}
	for index, grant := range r.Grants {
		t.Run("missing_"+grant.Kind+"_grant", func(t *testing.T) {
			bad := r.Clone()
			bad.Grants = append(bad.Grants[:index], bad.Grants[index+1:]...)
			documentationRefuses(t, bad, p, hooks)
		})
	}
	if p.Release {
		t.Run("missing_step_secret_paths", func(t *testing.T) {
			raw := strings.Replace(string(r.Document), "      metadata:\n        annotations:\n          oberth.ci/secret-paths: release/signing-key\n", "", 1)
			if strings.Contains(raw, SecretPathsAnnotation) {
				t.Fatal("example declaration not found")
			}
			_, err := prepareDocumentation(t, documentationDocument(r, []byte(raw)), p, hooks)
			requireCode(t, err, "secret_paths")
		})
	}
}

func TestDocumentationShellParameters(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/examples/build.yaml")
	if err != nil {
		t.Fatal(err)
	}
	shells := []string{"sh", "/bin/sh", "bash", "/bin/bash", "ash", "dash",
		"env, sh", "/usr/bin/env, sh", "env, /bin/bash"}
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			good := strings.Replace(string(raw), "[sh, -eu, -c]", "["+shell+", -eu, -c]", 1)
			if err := documentationShellParameters([]byte(good)); err != nil {
				t.Fatalf("quoted environment parameter refused: %v", err)
			}
			bad := strings.Replace(good, "$TRIGGER", "{{workflow.parameters.oberth-trigger}}", 1)
			if err := documentationShellParameters([]byte(bad)); err == nil {
				t.Fatal("shell parameter interpolation accepted")
			}
		})
	}
}

func TestDocumentationRefusalCommand(t *testing.T) {
	t.Parallel()
	w, fixture := tokenWorkflow()
	w.Spec.Entrypoint, w.Spec.Templates[0].Name = "publish", "publish"
	w.Spec.Templates[0].Volumes[0].Projected.Sources[0].ServiceAccountToken.Audience = "publisher"
	r := requestFor(t, w)
	r.Repository, r.SelectedFile = "forge/team/app", ".oberth/release.yaml"
	r.SourceReceipt.Revision.Repository, r.SourceReceipt.File = r.Repository, r.SelectedFile
	_, err := prepareDocumentation(t, r, fixture.Policy, identityHooks())
	findings := admissionFindings(t, err)
	page, err := os.ReadFile("../../docs/pipelines.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(page), "\\\n  ", "")
	for _, finding := range findings {
		if strings.Contains(finding.Command, "--kind identity") {
			if !strings.Contains(text, finding.Command) {
				t.Fatalf("documented command differs from admission: %s", finding.Command)
			}
			return
		}
	}
	t.Fatal("missing audience refusal command")
}
