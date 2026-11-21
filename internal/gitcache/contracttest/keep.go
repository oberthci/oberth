// Package contracttest exercises observable source retention against real and in-memory Git.
//
// Spec: docs/plans/c6-source-delivery.md#inspections
package contracttest

import (
	"context"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"strings"
	"testing"
)

type Retention struct {
	Keep func(context.Context, string, []sourcecontract.Binding) error
	Has  func(owner, repository, object string) bool
	Root sourcecontract.Binding
}

func Keep(t *testing.T, c Retention) {
	t.Helper()
	checkInputs(t, c)

	for range 2 {
		if err := c.Keep(t.Context(), "run-1", []sourcecontract.Binding{c.Root, c.Root}); err != nil {
			t.Fatal(err)
		}
	}
	if !c.Has("run-1", c.Root.Revision.Repository, c.Root.Revision.Commit) ||
		c.Has("run-2", c.Root.Revision.Repository, c.Root.Revision.Commit) {
		t.Fatal("owner retention mismatch")
	}
	if head := c.Root.Export.DefaultBranch.CommitSHA; head == "" || !c.Has("run-1", c.Root.Revision.Repository, head) {
		t.Fatal("recorded checkout head was not retained")
	}
	checkOwnObject(t, c)
	missing := c.Root
	missing.Export = sourcecontract.Export{}
	missing.Revision.Commit = strings.Repeat("f", 40)
	if err := c.Keep(t.Context(), "run-2", []sourcecontract.Binding{c.Root, missing}); err == nil {
		t.Fatal("missing root accepted")
	}
	if c.Has("run-2", c.Root.Revision.Repository, c.Root.Revision.Commit) {
		t.Fatal("partial repository transaction")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Keep(ctx, "run-3", []sourcecontract.Binding{c.Root}); err == nil {
		t.Fatal("canceled Keep accepted")
	}
}

func checkOwnObject(t *testing.T, c Retention) {
	t.Helper()
	raw := c.Root.Export.Ref.ObjectSHA
	if raw == c.Root.Revision.Commit || !c.Has("run-1", c.Root.Revision.Repository, raw) {
		t.Fatal("raw own tag was not retained")
	}
	missing := c.Root.Clone()
	missing.Export.Ref.ObjectSHA = strings.Repeat("f", 40)
	if err := c.Keep(t.Context(), "missing-tag", []sourcecontract.Binding{missing}); err == nil {
		t.Fatal("missing raw own tag accepted")
	}
	if c.Has("missing-tag", c.Root.Revision.Repository, c.Root.Revision.Commit) {
		t.Fatal("missing tag published partial roots")
	}
}

func checkInputs(t *testing.T, c Retention) {
	t.Helper()
	if err := c.Keep(t.Context(), "empty", nil); err != nil {
		t.Fatal("source-free retention refused", err)
	}
	if err := c.Keep(t.Context(), "../outside", []sourcecontract.Binding{c.Root}); err == nil {
		t.Fatal("invalid owner accepted")
	}
	invalid := c.Root
	invalid.Template = ""
	if err := c.Keep(t.Context(), "invalid", []sourcecontract.Binding{invalid}); err == nil {
		t.Fatal("invalid binding accepted")
	}

}
