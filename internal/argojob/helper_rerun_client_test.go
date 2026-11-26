package argojob

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRerunClientFencesPodUIDBeforeStaging(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	body, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	input := bytes.NewReader(body)
	code := r.client(t.Context(), []string{"rerun", "--pod-uid", "replaced-pod", "--sha256", digest}, HoldIO{In: input, Out: io.Discard, Err: io.Discard})
	if code != 120 || input.Len() != len(body) {
		t.Fatal("UID fence read or staged another Pod's snapshot", code, input.Len())
	}
}
