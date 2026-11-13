package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

func TestArgoSecurityDefaultsRenderNativeContexts(t *testing.T) {
	for _, stored := range []string{
		`{}`,
		`{"mainContainer":{"securityContext":{"runAsUser":0,"readOnlyRootFilesystem":false,"capabilities":{"add":["NET_BIND_SERVICE"]}}},"executor":{"securityContext":{"runAsUser":1000,"seccompProfile":{"type":"Localhost","localhostProfile":"profiles/operator.json"}}}}`,
		`{"mainContainer":{"securityContext":{"privileged":true,"allowPrivilegeEscalation":true,"capabilities":{"drop":[],"add":["SYS_ADMIN"]},"seccompProfile":{"type":"Unconfined"}}},"executor":{"securityContext":null}}`,
	} {
		t.Run(stored, func(t *testing.T) {
			var values map[string]any
			if err := json.Unmarshal([]byte(stored), &values); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(values)
			flags, review, err := argoSecurityDefaults(values)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(values)
			if !bytes.Equal(before, after) {
				t.Fatal("stored values mutated")
			}
			cfg := Config{argoSecurityArgs: flags, argoSecurityReview: review}
			objects := renderArgoHelmObjects(t, ArgoHelmArgs(cfg), values)
			found := false
			for _, object := range objects {
				if object["kind"] != "ConfigMap" {
					continue
				}
				data, _ := object["data"].(map[string]any)
				config, _ := data["config"].(string)
				if config == "" {
					continue
				}
				var native struct {
					MainContainer struct {
						SecurityContext *corev1.SecurityContext `json:"securityContext"`
					} `json:"mainContainer"`
					Executor struct {
						SecurityContext *corev1.SecurityContext `json:"securityContext"`
					} `json:"executor"`
				}
				if err := yaml.Unmarshal([]byte(config), &native); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(native.MainContainer.SecurityContext, review[0].Planned) || !reflect.DeepEqual(native.Executor.SecurityContext, review[1].Planned) {
					t.Fatalf("native chart differs from review: %s; review %+v", config, review)
				}
				found = true
			}
			if !found {
				t.Fatal("native controller config missing")
			}
			var printed bytes.Buffer
			PrintReview(&printed, cfg)
			if !strings.Contains(printed.String(), "mainContainer.securityContext:") || !strings.Contains(printed.String(), "executor.securityContext:") {
				t.Fatal(printed.String())
			}
			if stored == `{}` {
				for _, entry := range review {
					s, container := entry.Planned.(*corev1.SecurityContext)
					if !container {
						continue
					}
					if s == nil || s.AllowPrivilegeEscalation == nil || *s.AllowPrivilegeEscalation || s.Privileged == nil || *s.Privileged || s.SeccompProfile == nil || s.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || !slices.Equal(s.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(s.Capabilities.Add) != 0 || s.RunAsUser != nil || s.RunAsNonRoot != nil || s.ReadOnlyRootFilesystem != nil {
						t.Fatalf("default is not exactly the floor: %+v", s)
					}
				}
			}
		})
	}
}

func TestArgoSecurityDefaultsPreserveExplicitAncestors(t *testing.T) {
	for _, stored := range []string{`{"mainContainer":null,"executor":null}`, `{"mainContainer":{"securityContext":null},"executor":{"securityContext":null}}`, `{"mainContainer":{"securityContext":{"capabilities":null,"seccompProfile":null}},"executor":{"securityContext":{"capabilities":{"drop":[]},"seccompProfile":{"type":"Unconfined"}}}}`} {
		var values map[string]any
		if err := json.Unmarshal([]byte(stored), &values); err != nil {
			t.Fatal(err)
		}
		args, _, err := argoSecurityDefaults(values)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(args); i += 2 {
			if !strings.HasPrefix(args[i+1], "mainContainer.") && !strings.HasPrefix(args[i+1], "executor.") {
				continue
			}
			if strings.Contains(args[i+1], "capabilities") || strings.Contains(args[i+1], "seccomp") || (strings.Contains(stored, `"mainContainer":null`)) {
				t.Fatal("explicit operator value overwritten", args)
			}
		}
	}
}

func TestArgoSecurityReviewAndApplyReadStoredValues(t *testing.T) {
	stored := `{"mainContainer":{"securityContext":{"runAsUser":0,"readOnlyRootFilesystem":false}},"executor":{"securityContext":{"capabilities":{"add":["NET_BIND_SERVICE"]}}},"controller":{"config":{"archiveLogs":true}}}`
	client := fake.NewClientset(argoSecurityLiveObjects(DefaultArgoNamespace, false)...)
	var applied []string
	failRead := false
	deps := Deps{KubeClient: client, Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
		case "get":
			if slices.Contains(args, "--all") {
				t.Fatal("security defaults must use explicit stored values")
			}
			if failRead {
				return nil, errors.New("unreadable")
			}
			return []byte(stored), nil
		case "upgrade":
			applied = slices.Clone(args)
			setArgoSecurityLive(t, client, DefaultArgoNamespace, true)
		}
		return nil, nil
	}}
	cfg := Config{ArgoSecurityDefaults: true}
	if err := readArgoSecurityReview(t.Context(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	if len(Review(cfg).ArgoSecurity) != 6 {
		t.Fatal("review does not show Argo security contexts")
	}
	// An operator changes a value after review: apply must read it again.
	stored = `{"mainContainer":{"securityContext":{"allowPrivilegeEscalation":false,"runAsUser":0}},"executor":{"securityContext":{"seccompProfile":{"type":"Localhost","localhostProfile":"profiles/operator.json"}}}}`
	if _, err := InstallArgoWorkflows(t.Context(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied, "--reuse-values") || slices.Contains(applied, "mainContainer.securityContext.allowPrivilegeEscalation=false") || slices.Contains(applied, `executor.securityContext.seccompProfile.type="RuntimeDefault"`) {
		t.Fatal("operator value overwritten", applied)
	}
	if len(applied) == 0 {
		t.Fatal("missing defaults did not reconcile same-version release")
	}
	failRead = true
	applied = nil
	if _, err := InstallArgoWorkflows(t.Context(), cfg, deps); err == nil || len(applied) > 0 {
		t.Fatal("unreadable stored values allowed a write")
	}
}
