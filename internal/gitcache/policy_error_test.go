package gitcache

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/gitssh"
)

type failedPolicyTransport struct {
	gitBackend
	err      error
	attempts int
}

func (b *failedPolicyTransport) execute(ctx context.Context, spec commandSpec, capture bool) (string, error) {
	if len(spec.args) > 0 && spec.args[0] == "fetch" {
		b.attempts++
		return "", b.err
	}
	return b.gitBackend.execute(ctx, spec, capture)
}

func TestPolicyReadClassifiesOnlyTemporaryTransport(t *testing.T) {
	for _, name := range []string{"temporary", "authentication", "archived", "oversize", "configuration", "host key"} {
		t.Run(name, func(t *testing.T) {
			r := newTestRepository(t)
			r.commitFile(t, "policy", "policy.yaml", "required policy\n")
			c := newTestCache(t, r.upstream)
			if _, err := c.Ensure(t.Context(), "example"); err != nil {
				t.Fatal(err)
			}
			failed := &failedPolicyTransport{gitBackend: c.backend}
			limit := 100
			switch name {
			case "temporary":
				failed.err = errors.New(gitssh.TemporaryFailureMessage)
				c.backend = failed
			case "authentication":
				failed.err = errors.New("permission denied")
				c.backend = failed
			case "host key":
				failed.err = errors.New(gitssh.RefusalMessage)
				c.backend = failed
			case "archived":
				c.repositoryGate = func(context.Context, string) error { return errors.New("repository archived") }
			case "configuration":
				c.upstream = func(string) (string, error) { return "", errors.New("invalid remote configuration") }
			case "oversize":
				limit = 1
			}
			sha, body, exists, err := c.ReadDefaultBlob(t.Context(), "example", "policy.yaml", limit)
			if err == nil || errors.Is(err, ErrPolicyUpstreamUnavailable) != (name == "temporary") || exists || len(body) != 0 {
				t.Fatalf("policy error misclassified or returned authority: sha=%s exists=%v body=%q err=%v", sha, exists, body, err)
			}
			if name == "temporary" && failed.attempts != policyRefreshAttempts {
				t.Fatalf("temporary attempts=%d", failed.attempts)
			}
			if (name == "authentication" || name == "host key") && failed.attempts != 1 {
				t.Fatalf("permanent refusal retried %d times", failed.attempts)
			}
		})
	}
}
