package main

import (
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
)

func TestSystemStatusProbesStoreAfterStartup(t *testing.T) {
	c := testComposition(t)
	backend := &storefake.Store{Initialized: true}
	var err error
	c.storeWriter.Client, err = secretstore.New(secretstore.Connection{URL: "https://store.invalid", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	c.lifecycle = newLifecycle(t.Context(), []Part{{Name: "secret store"}})
	defer c.lifecycle.cancel()
	if err = c.lifecycle.start(); err != nil {
		t.Fatal(err)
	}
	c.health.setStarted(true)
	c.health.success = c.clock.Now()
	c.health.watch = func() bool { return true }
	admin := serverAdministration{&serverOwners{c: c}}
	for _, sealed := range []bool{false, true, false} {
		backend.Sealed = sealed
		status, err := admin.Status(t.Context(), "system_status")
		if err != nil {
			t.Fatal(err)
		}
		assertStoreReadiness(t, status, sealed)
	}
	assertReadinessCalls(t, backend.Calls)
}

func assertStoreReadiness(t *testing.T, status apicontract.SystemStatus, sealed bool) {
	t.Helper()
	if status.Ready == sealed || len(status.Parts) != 1 || status.Parts[0].Ready == sealed {
		t.Fatal("store restart hidden by startup state", status)
	}
	part := status.Parts[0]
	if sealed && (part.DisplayReason != "secret store sealed" || part.Remedy != "An administrator runs oberth unseal, then retry.") {
		t.Fatal("missing sealed remedy", part)
	}
	if !sealed && part.Remedy != "" {
		t.Fatal("stale remedy", part)
	}
}

func assertReadinessCalls(t *testing.T, calls []storefake.Call) {
	t.Helper()
	for _, call := range calls {
		if call.Path != "/sys/health" || call.Method != "GET" || call.Authenticated {
			t.Fatal("status read a secret or used credentials", call)
		}
	}
}
