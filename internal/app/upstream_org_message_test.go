package app

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// The mismatch is decided on the upstream's org, which is derived from its base
// URL, but the message named the upstream's own Name. When a deployment gives
// an upstream a short name and a base URL whose last segment differs, the two
// render the same string and the error reads "upstream \"local\", not
// \"local\"": it says a value is not itself, and the reader has nothing to act
// on.
func TestUpstreamOrgMismatchNamesTheValueThatWasCompared(t *testing.T) {
	t.Parallel()

	upstream := model.Upstream{Name: "local", BaseURL: "/data/local-upstreams"}
	if upstreamMatchesOrg(upstream, "local") {
		t.Fatal("org \"local\" must not match an upstream whose org is local-upstreams")
	}

	message := upstreamOrgMismatch("frag-lib", upstream, "local").Error()
	if !strings.Contains(message, "local-upstreams") {
		t.Errorf("message does not name the org that was actually compared: %s", message)
	}
	// The defect was the shape `upstream "X", not "X"`. Naming the upstream is
	// fine, and useful; asserting a value is not itself is not.
	if strings.Contains(message, `upstream "local", not "local"`) {
		t.Errorf("message still says a value is not itself: %s", message)
	}
	if !strings.Contains(message, `not "local"`) {
		t.Errorf("message does not name what the caller actually asked for: %s", message)
	}
}
