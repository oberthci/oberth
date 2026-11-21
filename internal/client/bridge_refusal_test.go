package client

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

// A refusal's required context, such as the findings of a refused rerun,
// reaches the agent whole and in order through the bridge.
func TestBridgeRefusalKeepsRequiredContext(t *testing.T) {
	refusal := publicerr.New(publicerr.InvalidInput, "pipeline admission failed", "Fix every reported finding, then push again.", nil)
	refusal.Context = []string{"refs/heads/x .oberth/build.yaml b: second.\n  fix: B\nrefs/heads/x .oberth/build.yaml a: first.\n  fix: A"}
	b := fake.Bindings()
	b.RunRerun.Handler = &fake.Handler[apicontract.RunRerunArgs, apicontract.Receipt]{Error: refusal}
	p := newBridgePeer(t, bridgeProfile(t, handlerTransport(server(t, b))))
	p.initialize()
	p.tool(7, "run_rerun", map[string]any{"id": "0123456789abcdef"}, nil)
	frame := p.frame()
	var reply struct {
		Result struct {
			IsError bool
			Content []struct{ Text string }
		}
	}
	var shown struct {
		Reason, Remedy string
		Context        []string
	}
	if json.Unmarshal(frame, &reply) != nil || !reply.Result.IsError || len(reply.Result.Content) != 1 || json.Unmarshal([]byte(reply.Result.Content[0].Text), &shown) != nil {
		t.Fatalf("bridge did not relay the refusal: %s", frame)
	}
	if shown.Reason != refusal.Reason() || shown.Remedy != refusal.Remedy() || !slices.Equal(shown.Context, refusal.Context) {
		t.Fatalf("bridge dropped the refusal context: %s", frame)
	}
}
