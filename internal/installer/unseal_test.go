package installer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// unsealTestDeps wires a store whose bao status is scripted and whose host
// secret store answers with key. LookPath is injected so the non-macOS branch
// finds secret-tool and the assertions hold on every platform.
func unsealTestDeps(t *testing.T, statusJSON, key string, keyErr error) (*fakeBaoRunner, Deps, *bytes.Buffer) {
	t.Helper()
	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"status -format=json": {out: statusJSON},
		"operator unseal":     {out: `{"initialized":true,"sealed":false,"storage_type":"file"}`},
	}}
	runner.secretStoreOut = key
	runner.secretStoreErr = keyErr
	var buf bytes.Buffer
	deps := secretStoreTestDeps(runner, &buf, runningOpenBaoPod())
	deps.LookPath = func(name string) (string, error) {
		if name == "secret-tool" {
			return "/usr/bin/secret-tool", nil
		}
		return "", errors.New("not found")
	}
	return runner, deps, &buf
}

// A sealed store after a pod restart is one command, not a remembered kubectl
// incantation plus a hunt for the key.
func TestUnsealSubmitsTheStoredKey(t *testing.T) {
	t.Parallel()
	const key = "storedunsealkey"
	runner, deps, out := unsealTestDeps(t, `{"initialized":true,"sealed":true,"storage_type":"file"}`, key, nil)

	if err := unsealStore(context.Background(), Config{Timeout: time.Second}, deps); err != nil {
		t.Fatal(err)
	}
	call, ok := runner.callsByCommand()["operator unseal"]
	if !ok {
		t.Fatal("the store was never asked to unseal")
	}
	if !strings.Contains(call.stdin, key) {
		t.Fatalf("the stored key did not reach the pod: stdin %q", call.stdin)
	}
	if !strings.Contains(out.String(), "unsealed") {
		t.Fatalf("no confirmation printed:\n%s", out.String())
	}
}

// Running it on a healthy store is a no-op that says so, rather than an error
// or a pointless unseal call.
func TestUnsealOnAnUnsealedStoreDoesNothing(t *testing.T) {
	t.Parallel()
	runner, deps, out := unsealTestDeps(t, `{"initialized":true,"sealed":false,"storage_type":"file"}`, "k", nil)

	if err := unsealStore(context.Background(), Config{Timeout: time.Second}, deps); err != nil {
		t.Fatal(err)
	}
	if _, ok := runner.callsByCommand()["operator unseal"]; ok {
		t.Fatal("an already-unsealed store was sent an unseal key anyway")
	}
	if !strings.Contains(out.String(), "already unsealed") {
		t.Fatalf("the no-op was not explained:\n%s", out.String())
	}
}

// An install from before the key was saved has no entry. The error has to name
// the store, the read command, and the path that still works today.
func TestUnsealWithoutAStoredKeyExplainsTheAlternative(t *testing.T) {
	t.Parallel()
	_, deps, _ := unsealTestDeps(t, `{"initialized":true,"sealed":true,"storage_type":"file"}`, "", errors.New("exit status 1"))

	err := unsealStore(context.Background(), Config{Timeout: time.Second}, deps)
	if err == nil {
		t.Fatal("a missing key was reported as success")
	}
	for _, want := range []string{"oberth-openbao-unseal", "bao operator unseal"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

// An uninitialized store is a different problem with a different fix.
func TestUnsealOnAnUninitializedStoreSaysSo(t *testing.T) {
	t.Parallel()
	_, deps, _ := unsealTestDeps(t, `{"initialized":false,"sealed":true,"storage_type":"file"}`, "k", nil)

	err := unsealStore(context.Background(), Config{Timeout: time.Second}, deps)
	if err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("err = %v, want the uninitialized-store guidance", err)
	}
}
