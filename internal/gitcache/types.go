package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"
)

// ErrUpstreamRefused indicates that upstream resolution was explicitly
// refused (for example, an org or upstream-name mismatch on a registered
// repository). The error is structurally different from a transient
// fetch failure: a refused resolution must never fall back to a stale
// cache, because the cache belongs to a different identity.
var ErrUpstreamRefused = errors.New("upstream resolution refused")

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
	recoveryOnly  bool
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
	// TargetUnborn marks a promotion whose upstream target branch does not
	// exist yet (brand-new repository, issue #264 bootstrap chain): BaseSHA
	// is empty and delivery creates the branch from the tested source.
	TargetUnborn bool
}

// PeeledObject preserves the raw tag/ref object identity used for publication
// while exposing the commit identity used for release ancestry and checkout.
// ObjectType is the Git object type string ("commit", "tag", "tree", "blob")
// returned by `git cat-file -t`; release admission uses it to enforce the
// annotated-tag provenance requirement.
type PeeledObject struct {
	ObjectSHA  string
	CommitSHA  string
	ObjectType string
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
// RepoQualification carries the upstream and org identity for a repository,
// used to determine the org-qualified cache directory path.
type RepoQualification struct {
	UpstreamName string
	Org          string
}

type Config struct {
	Root           string
	Upstream       func(repo string) (string, error)
	GitBinary      string
	CommandTimeout time.Duration
	Env            map[string]string
	Redact         []string
	Logger         Logger
	// RepoQualifier resolves a repository input — bare ("repo"),
	// org-qualified ("org/repo"), or fully qualified ("upstream/org/repo") —
	// to its canonical catalog identity. It determines the org-qualified
	// cache directory path (<root>/<upstream>/<org>/<repo>.git); the
	// returned values are the catalog's canonical spellings, never the
	// client's, so the on-disk path is stable across input casings.
	//
	// An error refuses the operation: an unknown repository resolves via
	// upstream discovery rules, and a bare name that exists under multiple
	// upstreams must be refused (not guessed) — the error should tell the
	// caller to qualify the path. When nil, the cache falls back to the
	// flat layout (<root>/<repo>.git); production wiring always sets it.
	RepoQualifier func(input string) (RepoQualification, error)
	// PreFinalizeGate is an optional server-owned check invoked after
	// receive-pack applies ref updates and BEFORE finalization records
	// the delta in the durable outbox. When it returns an error,
	// finalization and callback delivery are blocked; the reservation
	// persists in a gate-failed state preserving the original actor,
	// before-snapshot, and admission. Recovery replay re-runs the gate
	// for any not-yet-finalized reservation (both reserved and
	// gate-failed): a healthy gate finalizes with the original
	// attribution, a still-failing gate keeps the reservation for
	// future retry. Already-finalized (receiveReady) replayed
	// reservations bypass this gate; the per-callback recheck in the
	// handler still applies.
	PreFinalizeGate func(context.Context) error
}

// ServiceRunner is the narrow surface consumed by the SSH server.
type ServiceRunner interface {
	Ensure(context.Context, string) (Repository, error)
	Serve(context.Context, string, Service, string, io.Reader, io.Writer, io.Writer) error
	Receive(context.Context, string, string, string, io.Reader, io.Writer, io.Writer, ReceiveHandler) error
	ReplayPending(context.Context, ReceiveHandler) error
}

var _ Logger = (*log.Logger)(nil)
