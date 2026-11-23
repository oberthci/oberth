//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestEmptyForgeTopicWaitsForFirstMainPublication(t *testing.T) {
	for _, role := range []string{"administrator", "agent"} {
		t.Run(role, func(t *testing.T) { checkEmptyForgeTopic(t, role) })
	}
}

func checkEmptyForgeTopic(t *testing.T, role string) {
	t.Helper()
	f, first := firstEmptyForgePush(t)
	f.endNative(t, first, 1)
	f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunFailed })
	administrator := f.identity
	if role == "agent" {
		f.identity = f.joinBootstrapAgent(t)
		f.bindClient(t)
	}
	f.git.Write(t, ".oberth/build.yaml", []byte(composedWorkflow+"# fix tried on a topic\n"))
	fix := f.git.CommitFile(t, "try bootstrap fix on a topic")
	store := filepath.Join(filepath.Dir(f.config.DatabaseFile), "git", "origin", "team", "widget.git")
	gittest.ExpireRefresh(t, store)
	if output, err := f.pushResult(t, "refs/heads/fix", "-o", "oberth.run=1"); err != nil {
		t.Fatal("topic CI was refused", err, output)
	}
	topic := f.finishBootstrapRun(t, "refs/heads/fix")
	p := f.waitPublicationSettled(t, "publication:"+topic.ID)
	checkBootstrapTopicWait(t, p)
	f.retryBootstrapTopic(t, p.ID)
	checkBootstrapTopicWait(t, f.waitPublicationSettled(t, p.ID))
	if refs := f.git.Git(t, f.git.Bare, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatal("topic publication ended bootstrap", refs)
	}
	f.identity = administrator
	f.bindClient(t)
	if output, err := f.pushResult(t, "refs/heads/main"); err != nil {
		t.Fatal("topic CI blocked the first main publication", err, output)
	}
	main := f.finishBootstrapRun(t, "refs/heads/main")
	f.waitPublication(t, "publication:"+main.ID)
	if head := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main"); head != fix {
		t.Fatal("main did not publish the tested fix", head, fix)
	}
	f.retryBootstrapTopic(t, p.ID)
	f.waitPublication(t, p.ID)
	if head := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/fix"); head != fix {
		t.Fatal("topic did not publish after main", head, fix)
	}
}

func checkBootstrapTopicWait(t *testing.T, p model.Publication) {
	t.Helper()
	if p.Status != model.PublicationFailed || !strings.Contains(p.Detail, gitport.EmptyForgeRemedy) || p.RetryAt != nil {
		t.Fatal("topic publication did not wait for main with a remedy", p)
	}
	if p.PreviousSHA == nil || *p.PreviousSHA != "" {
		t.Fatal("topic's absent predecessor was not bound", p)
	}
}

func (f *composedFixture) retryBootstrapTopic(t *testing.T, id string) {
	t.Helper()
	var result json.RawMessage
	if err := f.administratorClient(t).Invoke(t.Context(), "publish_retry", apicontract.PublishRetryArgs{ID: id, Reason: "check default branch publication"}, &result); err != nil {
		t.Fatal("explicit topic publication retry refused", err)
	}
}

func (f *composedFixture) joinBootstrapAgent(t *testing.T) string {
	t.Helper()
	invitation := f.invitation(t, "bootstrap-agent")
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "joined")
	if _, err := client.Join(t.Context(), client.Config{Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS).Transport}, invitation, dir); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, client.UplinkPushKeyFile)
}

func (f *composedFixture) finishBootstrapRun(t *testing.T, ref string) stateport.Run {
	t.Helper()
	run := f.waitMatchingRun(t, func(r stateport.Run) bool {
		return r.Ref == ref && r.Status != stepmodel.RunFailed && r.Origin.WorkflowUID != ""
	})
	f.sourcePod(t, run)
	f.finishNative(t, run)
	return f.waitMatchingRun(t, func(r stateport.Run) bool { return r.ID == run.ID && r.Status == stepmodel.RunPassed })
}
