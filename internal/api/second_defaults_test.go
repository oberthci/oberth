package api_test

import (
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"testing"
)

func TestSecondTypedDefaultsDuplicatesRanges(t *testing.T) {
	f := setup(t, nil)
	r, _ := api.Builtin(f.bindings)
	for _, raw := range []string{`{"id":"12345678"}`, `{"id":"12345678","step_limit":null}`, `{"id":"12345678","step_limit":0}`} {
		if _, err := r.Invoke(t.Context(), admin(), "run_get", []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	in, _, _ := f.bindings.RunGet.Handler.(*fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]).Observed()
	for _, v := range in {
		if v.StepLimit != 100 {
			t.Error("default changed")
		}
	}
	for _, tc := range []struct {
		raw    string
		want   int
		refuse bool
	}{{`{"id":"12345678","step_limit":100}`, 100, false}, {`{"id":"12345678","step_limit":101}`, 0, true}, {`{"id":"12345678","step_limit":-1}`, 0, true}, {`{"id":"12345678","step_limit":1.5}`, 0, true}, {`{"id":"12345678","step_limit":9223372036854775808}`, 0, true}, {`{"id":"12345678","step_limit":""}`, 0, true}, {`{"id":"12345678","step_limit":1,"STEP_LIMIT":2}`, 2, false}, {`{"id":"12345678","step_limit":2,"step_limit":null}`, 2, false}} {
		_, err := r.Invoke(t.Context(), admin(), "run_get", []byte(tc.raw))
		if (err != nil) != tc.refuse {
			t.Errorf("%s: %v", tc.raw, err)
		}
		if !tc.refuse {
			v, _, _ := f.bindings.RunGet.Handler.(*fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]).Observed()
			if v[len(v)-1].StepLimit != tc.want {
				t.Error("duplicate standard semantics changed")
			}
		}
	}
	for _, tc := range []struct {
		raw     string
		nilBody bool
	}{{`{"id":1}`, true}, {`{"id":1,"body":null}`, true}, {`{"id":1,"body":""}`, false}} {
		_, err := r.Invoke(t.Context(), admin(), "issue_update", []byte(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		in, _, _ := f.bindings.IssueUpdate.Handler.(*fake.Handler[apicontract.IssueUpdateArgs, apicontract.IssueResult]).Observed()
		if (in[len(in)-1].Body == nil) != tc.nilBody {
			t.Error("empty body lost")
		}
	}
	t.Log("omission/null take defaults; empty issue body remains explicit; scalar duplicate uses last non-null value; overflow/fraction/negative/max+1 refused")
}
