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

func TestRepositoryRemovalWaitsForSecretWrite(t *testing.T) {
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	f.beforeWrite = func() { close(entered); <-release }
	done := make(chan error, 1)
	go func() { _, err := putSecret(t, p, q, []byte("private-delete-value")); done <- err }()
	awaitSecretBoundary(t, entered)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	call, err := p.f.s.PrepareRepoRemove(ctx, adminCaller(), apicontract.RepoRemoveArgs{Repo: repository})
	if _, err = execute(ctx, call, err); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("removal crossed an in-flight write", err)
	}
	unblock()
	awaitSecretCompletion(t, done)
	if err := removeSecretRepository(t, p); err != nil {
		t.Fatal(err)
	}
	if f.backend.Resources[storedPath(secretAccountFor(t, q, identityname.StepCI), q.Secret)] != nil {
		t.Fatal("in-flight value survived removal")
	}
}

func TestRepositoryRemovalExcludesNewSecretGrants(t *testing.T) {
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	f.before = sync.OnceFunc(func() { close(entered); <-release })
	done := make(chan error, 1)
	go func() { done <- removeSecretRepository(t, p) }()
	awaitSecretBoundary(t, entered)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	call, err := p.f.s.PrepareAccessAllow(ctx, adminCaller(), apicontract.AccessAllowArgs{Repo: repository, Workflow: q.Workflow, Step: q.Step, Secret: "new/path"})
	if _, err = execute(ctx, call, err); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("new secret grant crossed removal's snapshot", err)
	}
	unblock()
	awaitSecretCompletion(t, done)
	snapshot, err := p.state.Grants(t.Context(), []string{repository})
	if err != nil || len(snapshot.Grants) != 0 {
		t.Fatal("authority survived removal", snapshot, err)
	}
}
