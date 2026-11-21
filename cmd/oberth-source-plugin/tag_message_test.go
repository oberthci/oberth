package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"github.com/oberthci/oberth/internal/sourceplugin"
	"k8s.io/utils/clock"
)

func TestMissingRecordedTagMessageReachesRealPlugin(t *testing.T) {
	f := gittest.New(t)
	f.Git(t, f.Work, "tag", "-a", "recorded", "-m", "recorded", f.Commit)
	f.Git(t, f.Work, "push", f.Bare, "refs/tags/recorded")
	tag := f.Git(t, f.Work, "rev-parse", "refs/tags/recorded")
	cache := f.Cache(t, nil)
	b := f.Binding()
	b.Export.Tags = []sourcecontract.Ref{{Name: "refs/tags/recorded", ObjectSHA: tag, CommitSHA: f.Commit}}
	p := prepared(t, b)
	f.Git(t, f.Work, "tag", "-f", "-a", "recorded", "-m", "moved", f.Commit)
	f.Git(t, f.Work, "push", "--force", f.Bare, "refs/tags/recorded")
	gittest.ExpireRefresh(t, f.Store())
	if err := cache.Fetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.Store(), "FETCH_HEAD")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "gc", "--prune=now")
	caller := sourcedelivery.Caller{Namespace: "runs", Workflow: "wf", UID: "uid"}
	server := sourcedelivery.New(sourcedelivery.Config{Now: clock.RealClock{}.Now, SourceClientBurst: 30, Stage: cache.Stage,
		Authenticate: func(context.Context, string) (sourcedelivery.Caller, error) { return caller, nil },
		Run: func(context.Context, string) (sourcedelivery.Run, error) {
			return sourcedelivery.Run{Admission: p.View(), Namespace: caller.Namespace, Workflow: caller.Workflow, UID: caller.UID}, nil
		},
	})
	client, err := sourceplugin.New(pluginConfig(t, server))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	descriptor, err := json.Marshal(b.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	input := &artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: "run-1", Configuration: string(descriptor)}}
	err = client.Load(t.Context(), input, filepath.Join(t.TempDir(), "source.tgz"))
	if err == nil || !strings.Contains(err.Error(), "tag moved since admission") {
		t.Fatal("decided tag message did not reach plugin", err)
	}
}
