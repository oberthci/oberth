package contracttest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/gitport"
)

type GitFixture struct {
	Port     cronsync.Git
	Revision gitport.Revision
	Ref      gitport.Ref
	File     gitport.File
}

func RunGit(t *testing.T, create func(*testing.T) GitFixture) {
	t.Helper()
	f := create(t)
	ref, err := f.Port.ResolveRef(t.Context(), f.Revision.Repository, f.Ref.Name)
	if err != nil || ref != f.Ref {
		t.Fatal("exact ref differs", ref, err)
	}
	file, err := f.Port.ReadFile(t.Context(), f.Revision, f.File.Path)
	if err != nil || !reflect.DeepEqual(file, f.File) {
		t.Fatal("exact file differs", file, err)
	}
	file.Bytes[0] ^= 1
	again, err := f.Port.ReadFile(t.Context(), f.Revision, f.File.Path)
	if err != nil || !reflect.DeepEqual(again, f.File) {
		t.Fatal("file aliases storage", err)
	}
	_, err = f.Port.ReadFile(t.Context(), f.Revision, "missing.yaml")
	var absent *gitport.AbsentError
	if !errors.As(err, &absent) || absent.Path != "missing.yaml" {
		t.Fatal("absence is not a file result", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = f.Port.ReadFile(ctx, f.Revision, f.File.Path); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	if _, err = f.Port.ResolveRef(ctx, f.Revision.Repository, f.Ref.Name); !errors.Is(err, context.Canceled) {
		t.Fatal("ref cancellation lost", err)
	}
}
