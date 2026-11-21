package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/identityname"
)

func TestSecretRevokeWaitsForInFlightPut(t *testing.T) {
	t.Parallel()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	f.beforeWrite = func() { close(entered); <-release }
	ctx := apicontract.WithSecretValue(t.Context(), []byte("private-delete-value"))
	put, err := p.f.s.PrepareSecretPut(ctx, adminCaller(), q)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := put(ctx); done <- err }()
	awaitSecretBoundary(t, entered)
	revokeCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	call, err := p.f.s.PrepareAccessRevoke(revokeCtx, adminCaller(), apicontract.AccessRevokeArgs{Repo: q.Repo, Workflow: q.Workflow, Step: q.Step, Secret: q.Secret})
	_, err = execute(revokeCtx, call, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("revoke crossed an in-flight put", err)
	}
	assertGrantState(t, p, q, false)
	if deleteCalls(f) != 0 {
		t.Fatal("delete raced the earlier write")
	}
	unblock()
	awaitSecretCompletion(t, done)
	result, err := revokeSecret(t, p, q)
	account := secretAccountFor(t, q, identityname.StepCI)
	if err != nil || result.State != "revoked" || f.backend.Resources[storedPath(account, q.Secret)] != nil {
		t.Fatal("completed put survived revocation", result, err)
	}
}

func TestSecretPutWaitsForInFlightRevoke(t *testing.T) {
	t.Parallel()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	account := secretAccountFor(t, q, identityname.StepCI)
	seedSecret(t, f, account, q.Secret)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	f.before = sync.OnceFunc(func() { close(entered); <-release })
	done := make(chan error, 1)
	go func() { _, err := revokeSecret(t, p, q); done <- err }()
	awaitSecretBoundary(t, entered)
	putCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	putCtx = apicontract.WithSecretValue(putCtx, []byte("private-delete-value"))
	call, err := p.f.s.PrepareSecretPut(putCtx, adminCaller(), q)
	_, err = execute(putCtx, call, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("put crossed an in-flight revoke", err)
	}
	assertGrantState(t, p, q, true)
	unblock()
	awaitSecretCompletion(t, done)
	if _, err := putSecret(t, p, q, []byte("private-delete-value")); err == nil {
		t.Fatal("late put resurrected revoked value")
	}
	if f.backend.Resources[storedPath(account, q.Secret)] != nil {
		t.Fatal("revoked value was recreated")
	}
}

func awaitSecretBoundary(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("secret operation did not reach its boundary")
	}
}

func awaitSecretCompletion(t *testing.T, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("secret operation did not complete")
	}
}
