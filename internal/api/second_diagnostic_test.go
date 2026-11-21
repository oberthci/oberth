package api_test

import (
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"strings"
	"testing"
)

func TestClassifiedDependencyNeverReachesFixedDiagnostics(t *testing.T) {
	dependency := errors.New("private-dependency-marker")
	f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
		c.Classify = func(err error) *publicerr.Error {
			if errors.Is(err, dependency) {
				return publicerr.New(publicerr.Unavailable, "Storage is temporarily unavailable.", "Retry shortly.", err)
			}
			return publicerr.Classify(err)
		}
		b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Error: dependency}
	})
	w := rpc(f, "run_wait", map[string]string{"id": "12345678"})
	if strings.Contains(w.Body.String(), dependency.Error()) {
		t.Fatal("classified dependency reached caller")
	}
	items := f.diagnostics.Items()
	if len(items) != 1 || items[0].Outcome != api.Failed {
		t.Fatal("fixed diagnostic contains a non-catalogue outcome", items)
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), dependency.Error()) {
		t.Fatal("classified dependency reached diagnostics")
	}
}
