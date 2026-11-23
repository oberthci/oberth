package main

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestPromoteWithoutCheckoutExplainsFullID(t *testing.T) {
	for _, ref := range []string{"abcdef0", "topic", "HEAD", strings.Repeat("x", 100)} {
		t.Run(ref, func(t *testing.T) {
			bindings := fake.Bindings()
			bindings.Promote.Handler = &fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Run: func(context.Context) (apicontract.PromoteResult, error) {
				t.Fatal("unresolved local ref reached the server")
				return apicontract.PromoteResult{}, nil
			}}
			app, _, diagnostic := fixture(t, bindings)
			app.directory = t.TempDir()
			if code := app.run(t.Context(), []string{"promote", ref, "--repo", "project"}); code != 2 {
				t.Fatal("local resolution did not refuse", code, diagnostic.String())
			}
			text := strings.Join(strings.Fields(diagnostic.String()), " ")
			for _, want := range []string{"Cannot resolve a local commit.", "Commit: " + ref, "Use the full commit ID with --repo, or run inside a checkout."} {
				if !strings.Contains(text, want) {
					t.Fatal("missing checkout-free remedy or commit context", diagnostic.String())
				}
			}
		})
	}
}
