package contracttest

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func checkLookupRecord(t *testing.T, f RunFixture) {
	r := acceptOneRun(t, f, RunCase{ID: "record-one"})
	for _, selector := range []string{r.ID, "record"} {
		got, err := f.Port.LookupRun(t.Context(), []string{r.Repo}, selector)
		mustRun(t, err)
		if got.ID != r.ID || got.Revision != r.Revision || !got.HasAdmission || got.Admission.Digest != "" || got.Admission.Workflow != nil {
			t.Fatal("record lookup loaded or lost admission metadata", got)
		}
		_, err = f.Port.LookupRun(t.Context(), []string{}, selector)
		runCode(t, err, publicerr.NotFound)
	}
	full, err := f.Port.Run(t.Context(), r.ID)
	mustRun(t, err)
	if full.Admission.Digest == "" {
		t.Fatal("record lookup removed the durable admission")
	}
	_, err = f.Port.LookupRun(t.Context(), nil, "")
	runCode(t, err, publicerr.InvalidInput)
}
