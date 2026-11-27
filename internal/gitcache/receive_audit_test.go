package gitcache_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

type receiveAuditCounts struct{ calls, delivered int }

func TestReceiveAuditsAdmissionAndHookRefusalsOnce(t *testing.T) {
	for _, shape := range []string{"admission", "default", "accepted", "audit-failure", "cancelled"} {
		t.Run(shape, func(t *testing.T) { checkReceiveAuditCase(t, shape) })
	}
}

func checkReceiveAuditCase(t *testing.T, shape string) {
	t.Helper()
	f := gittest.New(t)
	f.Write(t, "next.txt", []byte("next\n"))
	next := f.CommitFile(t, "next")
	ref, old := "refs/heads/topic", ""
	if shape == "default" {
		ref, old = "refs/heads/main", f.Commit
	}
	updates := []gitcache.RefUpdate{{Ref: ref, OldSHA: old, NewSHA: next}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	counts := &receiveAuditCounts{}
	problem, failedAudit := errors.New("fixture admission refusal"), errors.New("fixture audit failure")
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
			if shape == "accepted" {
				return nil
			}
			if shape == "cancelled" {
				cancel()
			}
			return problem
		}
		cfg.Refused = func(ctx context.Context, e gitcache.ReceiveEvent, err error) error {
			counts.calls++
			if ctx.Err() != nil {
				t.Error("refusal lost trusted metadata", ctx.Err())
			}
			checkReceiveAuditMetadata(t, e, updates, err)
			if shape == "audit-failure" {
				return failedAudit
			}
			return nil
		}
	})
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveBatchInput(t, f, updates, nil)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { counts.delivered++; return nil, nil }
	err := c.Receive(ctx, r)
	checkReceiveAuditResult(t, shape, counts, err)
	if shape == "audit-failure" && !errors.Is(err, failedAudit) {
		t.Fatal("audit failure was hidden", err)
	}
}

func checkReceiveAuditMetadata(t *testing.T, e gitcache.ReceiveEvent, updates []gitcache.RefUpdate, cause error) {
	t.Helper()
	if e.Actor != "agent" || e.Repository != gittest.Repository || !reflect.DeepEqual(e.Updates, updates) || cause == nil {
		t.Error("refusal lost trusted metadata", e, cause)
	}
}

func checkReceiveAuditResult(t *testing.T, shape string, counts *receiveAuditCounts, err error) {
	t.Helper()
	if shape == "accepted" {
		if err != nil || counts.calls != 0 || counts.delivered != 1 {
			t.Fatal("accepted receive changed", err, counts)
		}
		return
	}
	if err == nil || counts.calls != 1 || counts.delivered != 0 {
		t.Fatal("refusal audit result differs", err, counts)
	}
}
