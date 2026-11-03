package installer

import (
	"context"
	"errors"
	"testing"
)

func TestIdentityDeltaKeepsFreshAndSuccessfulPaths(t *testing.T) {
	for _, tc := range []struct {
		count int
		live  map[string]any
		err   error
	}{
		{0, nil, errors.New("fresh install has no server")},
		{1, map[string]any{"argo": map[string]any{"perRepoIdentities": []any{"existing"}}}, nil},
	} {
		if err := checkProducedIdentities(tc.live, "perRepoIdentities", tc.count, tc.err); err != nil {
			t.Fatal(err)
		}
	}
}
func TestLiveIdentityReadCannotSwallowHelmFailure(t *testing.T) {
	broken := errors.New("helm unavailable")
	_, err := readHelmValuesMap(t.Context(), Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return nil, broken }}, "oberth")
	if !errors.Is(err, broken) {
		t.Fatalf("read failure=%v", err)
	}
}
