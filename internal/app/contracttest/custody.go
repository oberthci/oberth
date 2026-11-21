package contracttest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

type KeeperFixture struct {
	Port     app.Keeper
	Bindings func(string) []sourcecontract.Binding
	Binding  sourcecontract.Binding
}

func Keeper(t *testing.T, setup func(*testing.T) KeeperFixture) {
	t.Helper()
	t.Run("keeper", func(t *testing.T) {
		f := setup(t)
		bindings := []sourcecontract.Binding{f.Binding.Clone()}
		for range 2 {
			if err := f.Port.Keep(context.Background(), "owner", bindings); err != nil {
				t.Fatal(err)
			}
		}
		bindings[0].Revision.Commit = "mutated"
		if !reflect.DeepEqual(f.Bindings("owner"), []sourcecontract.Binding{f.Binding}) {
			t.Fatal("retained bindings differ")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := f.Port.Keep(ctx, "cancelled", bindings); err == nil {
			t.Fatal("cancelled Keep succeeded")
		}
		if len(f.Bindings("cancelled")) != 0 {
			t.Fatal("cancelled Keep changed bindings")
		}
	})
}

type SourceFixture struct {
	Port                    app.Source
	First, Second           sourcecontract.Revision
	File                    string
	FirstBytes, SecondBytes []byte
	Ref                     sourcecontract.Ref
	ProducerID              int64
}

func Source(t *testing.T, setup func(*testing.T) SourceFixture) {
	t.Helper()
	t.Run("source", func(t *testing.T) {
		f := setup(t)
		sourceFiles(t, f)
		_, err := f.Port.ReadFile(context.Background(), f.First, ".oberth/missing.yaml")
		var absent *gitport.AbsentError
		if !errors.As(err, &absent) || absent.Path != ".oberth/missing.yaml" {
			t.Fatal("missing file is not typed", err)
		}
		snapshot, err := f.Port.Checkout(context.Background(), f.First.Repository, f.Ref)
		if err != nil || snapshot.Ref != f.Ref {
			t.Fatal(snapshot, err)
		}
		head, err := f.Port.Checkout(context.Background(), f.First.Repository, sourcecontract.Ref{})
		if err != nil || head.Ref.Name == "" || head.Ref != head.DefaultBranch {
			t.Fatal("default checkout not one exact head", head, err)
		}
		id, reachable, err := f.Port.Pin(context.Background(), f.Second)
		if err != nil || !reachable || id != f.ProducerID {
			t.Fatal(id, reachable, err)
		}
	})
}

type AuthorityFixture struct {
	Port  app.Authority
	Scope app.Scope
}

func Authority(t *testing.T, setup func(*testing.T) AuthorityFixture) {
	t.Helper()
	t.Run("authority", func(t *testing.T) {
		f := setup(t)
		q, p, err := f.Port.Load(context.Background(), f.Scope)
		if err != nil {
			t.Fatal(err)
		}
		if p.Release != f.Scope.Release || q.IdentityRequest.Namespace == "" {
			t.Fatal("unscoped authority")
		}
		q.IdentityRequest.PipelineAccount = "mutated"
		again, _, err := f.Port.Load(context.Background(), f.Scope)
		if err != nil || again.IdentityRequest.PipelineAccount == "mutated" {
			t.Fatal("authority snapshot aliases state", err)
		}
	})
}

func sourceFiles(t *testing.T, f SourceFixture) {
	t.Helper()

	for _, r := range []struct {
		at    sourcecontract.Revision
		bytes []byte
	}{{f.First, f.FirstBytes}, {f.Second, f.SecondBytes}} {
		got, err := f.Port.ReadFile(context.Background(), r.at, f.File)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Bytes, r.bytes) {
			t.Fatal("file not read at exact commit")
		}
		got.Bytes[0] ^= 1
		again, err := f.Port.ReadFile(context.Background(), r.at, f.File)
		if err != nil || !reflect.DeepEqual(again.Bytes, r.bytes) {
			t.Fatal("file bytes alias")
		}
	}
}
