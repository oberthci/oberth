//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestTopicOwnPinReadsHealthyForgeAfterCacheTrustExpires(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	seed := f.prepareCommit(t)
	var settings serverRuntime
	if err := json.Unmarshal(f.config.Runtime, &settings); err != nil {
		t.Fatal(err)
	}
	settings.SourcePolicy = json.RawMessage(`{"origin/team/widget":["origin/team/widget"]}`)
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	f.config.Runtime = raw
	descriptor := fmt.Sprintf(`{"version":1,"kind":"pin","repository":%q,"commit":%q}`, gittest.Repository, seed)
	document := strings.ReplaceAll(composedWorkflow, "/work/src", "/work/sources/lib")
	document = strings.Replace(document, `{"version":1,"kind":"main"}`, descriptor, 1)
	f.git.Write(t, ".oberth/build.yaml", []byte(document))
	f.git.CommitFile(t, "own pin")
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.enrollJoined(t)
	f.bindClient(t)
	setPushCacheAge(t, f, 16*time.Minute)
	before := f.forge.sessions.Load()
	if output, err := f.pushResult(t, "refs/heads/topic", "-o", "oberth.run=1"); err != nil {
		t.Fatal("old cache refused an own pin while its forge answered", output, err)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if len(run.Admission.SourceBindings) != 1 || run.Admission.SourceBindings[0].Revision.Commit != seed {
		t.Fatal("own pin bound another revision", run.Admission.SourceBindings)
	}
	if f.forge.sessions.Load() <= before {
		t.Fatal("own pin did not read its healthy forge after the cache expired")
	}
}
