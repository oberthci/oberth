package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	chartfake "github.com/oberthci/oberth/internal/installer/fake"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

type restoreCluster struct {
	engine  *installer.Engine
	charts  *chartfake.Chart
	client  *kfake.Clientset
	release installer.Release
	choices installer.Choices
}

func restoreClusterFixture(t *testing.T) restoreCluster {
	t.Helper()
	engine, charts := offlineInstallEngine(t, t.TempDir())
	release, err := installer.LocalRelease("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	return restoreCluster{engine, charts, engine.Cluster.Client.(*kfake.Clientset), release, installer.Choices{Context: "chosen"}}
}

func installRestoreSource(t *testing.T, f restoreCluster) installer.Result {
	t.Helper()
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	result, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func nativeSnapshotFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := storefake.Snapshot([]byte("snapshot-private-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func configureRestoreSnapshot(t *testing.T, f restoreCluster) {
	t.Helper()
	f.client.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{Token: "backup-projection-fixture"}}, nil
	})
	backend := &storefake.Store{Initialized: true, Snapshot: nativeSnapshotFixture(t)}
	f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: backend})
		return client, func() {}, err
	}
}

func restoreJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertNoRestoreWrites(t *testing.T, f restoreCluster) {
	t.Helper()
	for _, action := range f.client.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" && !chartfake.DryRunService(action) {
			t.Fatal("refused restore wrote to the cluster", action.GetResource())
		}
	}
}
