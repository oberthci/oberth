//go:build linux

// These helpers exercise Linux server composition.

package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/nativebinding"
)

func (f *composedFixture) checkProtectedNamespaces(t *testing.T) {
	t.Helper()
	q := apicontract.Capability{ServiceAccount: "resource", ResourceKinds: []apicontract.ResourceKind{{APIVersion: "v1", Kind: "ConfigMap", Actions: []string{"create"}}}}
	for _, namespace := range []string{f.config.Namespace, f.config.ArgoNamespace, "unprotected-runs"} {
		q.Namespace = namespace
		err := f.owners.authority.ValidateCapability(t.Context(), q)
		if (err == nil) != (namespace == "unprotected-runs") {
			t.Fatal("wrong protected namespace policy", namespace, err)
		}
	}
	_, policy, err := f.owners.authority.Load(t.Context(), app.Scope{Mode: admissioncontract.ModeInspect, Preparation: nativebinding.Preparation{Repository: gittest.Repository, File: ".oberth/build.yaml"}})
	if err != nil || policy.ServerNamespace != f.config.Namespace || policy.ArgoNamespace != f.config.ArgoNamespace || f.owners.native.Hooks(nativebinding.Preparation{}).LoginAudience != f.config.SecretStore.Audience {
		t.Fatal("admission did not receive configured namespaces and store audience", policy, err)
	}
	for _, action := range []string{"admin_version", "system_status"} {
		result, err := (serverAdministration{f.owners}).Status(t.Context(), action)
		if err != nil || result.Version != strings.Fields(buildVersion())[1] {
			t.Fatal("server version differs from the binary build version", action, result.Version, err)
		}
	}
	f.owners.admissionMetric("admission timeout")
	if !strings.Contains(f.owners.c.health.metrics(t.Context()), "oberth_admission_timeouts_total 1\n") || !strings.Contains(f.owners.c.health.metrics(t.Context()), "oberth_executor_incomplete_evidence_total 0\n") {
		t.Fatal("owner observations are not exported")
	}
}
