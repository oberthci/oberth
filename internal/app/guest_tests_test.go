package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type guestGrantLoader struct {
	budget  store.KubeVirtBudget
	allowed bool
	err     error
	ids     []int64
}

func (loader *guestGrantLoader) ActiveKubeVirtGrant(_ context.Context, id int64) (store.KubeVirtBudget, bool, error) {
	loader.ids = append(loader.ids, id)
	return loader.budget, loader.allowed, loader.err
}

const guestTestDocument = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/kvm-templates: main
    oberth.ci/guest-test-templates: main
spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  templates:
  - name: main
    automountServiceAccountToken: false
    metadata:
      annotations:
        oberth.ci/workspace-mounts: none
        oberth.ci/workspace-env: none
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: ["true"]
`

func TestGuestTestsReadLiveGrantAndAuditBeforeCreate(t *testing.T) {
	fixture := newRaceFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.sourceDir, ".oberth", "build.yaml"), []byte(guestTestDocument), 0644); err != nil {
		t.Fatal(err)
	}
	fixture.jobs.config.KVMEnabled = true
	loader := &guestGrantLoader{allowed: true}
	fixture.jobs.SetKubeVirtGrantLoader(loader)
	created, audited := 0, 0
	fixture.jobs.auditor = &stubAuditor{onAppend: func(spec model.AuditActionSpec) {
		var details map[string]any
		if err := json.Unmarshal([]byte(spec.Details), &details); err != nil {
			t.Fatal(err)
		}
		if details["guest_test_templates"] != "main" || details["kubevirt_granted"] != true || details["repo_id"] != float64(42) || details["sha"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Fatalf("guest audit: %v", details)
		}
		audited++
	}}
	fixture.jobs.controller = &orderedNonrootController{argoControl: fixture.controller,
		prepare: func(request argojob.Request) (argojob.Request, error) { return request, nil },
		create: func(request argojob.Request) (string, error) {
			if audited != created+1 {
				t.Fatal("create before audit")
			}
			created++
			return request.Name, nil
		},
	}
	for i, name := range []string{"granted", "revoked", "unreadable"} {
		request := fixture.request("wf-guest-"+name, "run-guest-"+name)
		request.Repository.ID = 42
		if i == 1 {
			loader.allowed = false
		}
		if i == 2 {
			loader.err = errors.New("grant database unavailable")
		}
		err := fixture.jobs.CreateCI(t.Context(), request)
		if (err == nil) != (i == 0) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if created != 1 || audited != 1 || len(loader.ids) != 3 {
		t.Fatalf("created=%d audited=%d loads=%v", created, audited, loader.ids)
	}
	for _, id := range loader.ids {
		if id != 42 {
			t.Fatalf("grant used wrong repository ID %d", id)
		}
	}
}
