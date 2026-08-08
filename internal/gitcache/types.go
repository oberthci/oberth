package gitcache

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"
)

// Service is one of the two Git smart-protocol programs exposed by Oberth.
type Service string

const (
	UploadPack  Service = "git-upload-pack"
	ReceivePack Service = "git-receive-pack"
)

// ParseService rejects every command outside the Git smart-protocol allowlist.
func ParseService(value string) (Service, error) {
	switch Service(value) {
	case UploadPack:
		return UploadPack, nil
	case ReceivePack:
		return ReceivePack, nil
	default:
		return "", fmt.Errorf("git service %q is not allowed", value)
	}
}

// Repository is a ready bare cache. Stale is true when refresh failed but a
// previously populated cache remains safe to serve.
type Repository struct {
	Path          string
	Stale         bool
	DefaultBranch string
}

// RefUpdate describes one public branch or tag change.
type RefUpdate struct {
	Ref    string `json:"ref"`
	OldSHA string `json:"old_sha,omitempty"`
	NewSHA string `json:"new_sha,omitempty"`
}

func (u RefUpdate) Deleted() bool { return u.NewSHA == "" }

func (u RefUpdate) Kind() string {
	switch {
	case len(u.Ref) > len("refs/heads/") && u.Ref[:len("refs/heads/")] == "refs/heads/":
		return "branch"
	case len(u.Ref) > len("refs/tags/") && u.Ref[:len("refs/tags/")] == "refs/tags/":
		return "tag"
	default:
		return "unknown"
	}
}

// MergeCandidate is the immutable result to test and eventually publish for a
// promotion. BaseSHA is the exact upstream target observed before the merge.
type MergeCandidate struct {
	BaseSHA     string
	MergedSHA   string
	FastForward bool
}

// PeeledObject preserves the raw tag/ref object identity used for publication
// while exposing the commit identity used for release ancestry and checkout.
type PeeledObject struct {
	ObjectSHA string
	CommitSHA string
}

// ReleaseAdmission identifies the exact fresh upstream default-branch
// snapshot that guarded a receive. The SHA, rather than later repository
// metadata, is the durable release ancestry authority.
type ReleaseAdmission struct {
	DefaultBranch string `json:"default_branch"`
	SHA           string `json:"sha"`
}

// Logger is deliberately smaller than log.Logger and never receives command
// output or environment values.
type Logger interface {
	Printf(format string, args ...any)
}

// ReceiveEvent is one durably reserved, actor-bound public ref change. ID is
// stable across callback retries and process restarts so handlers can make
// their own durable side effects idempotent.
type ReceiveEvent struct {
	ID               string           `json:"id"`
	Actor            string           `json:"actor"`
	Repo             string           `json:"repo"`
	Update           RefUpdate        `json:"update"`
	ReleaseAdmission ReleaseAdmission `json:"release_admission"`
}

// ReceiveHandler records a committed receive event. Implementations must use
// ReceiveEvent.ID as their idempotency key because a crash after a successful
// callback but before the outbox checkpoint can replay the same event.
type ReceiveHandler interface {
	HandleReceive(context.Context, ReceiveEvent) error
}

type ReceiveHandlerFunc func(context.Context, ReceiveEvent) error

func (f ReceiveHandlerFunc) HandleReceive(ctx context.Context, event ReceiveEvent) error {
	return f(ctx, event)
}

// PendingReceive identifies one durable per-repository reservation. It is a
// startup diagnostic and replay primitive, not a mutation API.
type PendingReceive struct {
	ID        string `json:"id"`
	Repo      string `json:"repo"`
	Actor     string `json:"actor"`
	State     string `json:"state"`
	Remaining int    `json:"remaining"`
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

// Config constructs a cache rooted at Root. Upstream resolves a validated
// repository name to a Git remote. Env is an operator-owned allowlist of
// variables required by Git (for example, a fixed GIT_SSH_COMMAND).
type Config struct {
	Root           string
	Upstream       func(repo string) (string, error)
	GitBinary      string
	CommandTimeout time.Duration
	Env            map[string]string
	Redact         []string
	Logger         Logger
}

// ServiceRunner is the narrow surface consumed by the SSH server.
type ServiceRunner interface {
	Ensure(context.Context, string) (Repository, error)
	Serve(context.Context, string, Service, string, io.Reader, io.Writer, io.Writer) error
	Receive(context.Context, string, string, string, io.Reader, io.Writer, io.Writer, ReceiveHandler) error
	ReplayPending(context.Context, ReceiveHandler) error
}

var _ Logger = (*log.Logger)(nil)
