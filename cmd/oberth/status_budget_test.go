package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/store"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestSystemStatusBlockedProbesShareRequestBudget(t *testing.T) {
	c := testComposition(t)
	started := make(chan string, 2)
	transport := testTransport(func(r *http.Request) (*http.Response, error) {
		started <- r.URL.Host
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	var err error
	c.storeWriter.Client, err = secretstore.New(secretstore.Connection{URL: "https://store.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: "https://cluster.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	c.store, err = store.Create(t.Context(), t.TempDir()+"/state.db", store.DefaultOptions(c.clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.store.Close() })
	c.lifecycle = newLifecycle(t.Context(), []Part{{Name: "secret store"}})
	defer c.lifecycle.cancel()
	if err = c.lifecycle.start(); err != nil {
		t.Fatal(err)
	}
	c.health.setStarted(true)
	c.health.success = c.clock.Now()
	a := serverAdministration{&serverOwners{c: c, cluster: clusterClients{Kube: kube}}}
	// Leave two seconds of the service's ten-second budget for its remaining reads.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	status, err := a.Status(ctx, "system_status")
	if err != nil || ctx.Err() != nil {
		t.Fatal("blocked probes spent the request budget", err, ctx.Err())
	}
	if _, _, err = c.store.StateMetadata(ctx); err != nil {
		t.Fatal("admin metadata lost its remaining budget", err)
	}
	if len(started) != 2 || status.Ready || len(status.Parts) != 1 || status.Parts[0].DisplayReason != "secret store unavailable" {
		t.Fatal("blocked probes did not report both observations", status, len(started))
	}
	assertBlockedControllerSetting(t, status)
}

func assertBlockedControllerSetting(t *testing.T, status apicontract.SystemStatus) {
	t.Helper()
	if status.Settings != nil {
		for _, row := range *status.Settings {
			if row.Name == apicontract.SettingParallelRuns && row.Value == "unavailable" && row.Source == "" {
				return
			}
		}
	}
	t.Fatal("blocked controller observation missing", status.Settings)
}
