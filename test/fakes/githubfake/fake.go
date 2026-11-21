// Package githubfake serves the inventoried upstream calls using real Git.
// Spec: docs/plans/README.md#principles
package githubfake

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Config isolates all Git repositories beneath Root. HostKeys may provide
// repeatable pins; otherwise all three supported key families are generated.
// Now supplies handshake time; Delay may replace the cancellable timer.
type Config struct {
	Root     string
	Logger   *log.Logger
	HostKeys []ssh.Signer
	Now      func() time.Time
	Delay    func(context.Context, time.Duration) bool
}

// Fake owns in-memory controls; Git alone owns on-disk objects and refs.
type Fake struct {
	mu         sync.Mutex
	root       string
	now        func() time.Time
	delay      func(context.Context, time.Duration) bool
	signers    []ssh.Signer
	logger     *log.Logger
	repos      map[string][]DeployKey
	authorized []string
	assets     map[string][]byte
	faults     []Fault
	calls      []Call
	unexpected int
	nextID     int64
}

// DeployKey is the subset returned by the deploy-key API.
type DeployKey struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	Title     string `json:"title"`
	ReadOnly  bool   `json:"read_only"`
	Verified  bool   `json:"verified"`
	CreatedAt string `json:"created_at"`
	URL       string `json:"url"`
}

// Seed is additive. It never replaces refs, keys or existing repositories.
type Seed struct {
	Repositories   []RepositorySeed `json:"repositories"`
	AuthorizedKeys []string         `json:"authorized_keys"`
	Assets         []Asset          `json:"assets"`
}
type RepositorySeed struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
}
type Asset struct {
	Path  string `json:"path"`
	Bytes []byte `json:"bytes"` // JSON base64; no upstream fetching.
}

// Fault matches an HTTP method and path, or an SSH operation and repository.
// Operations are auth, git-upload-pack and git-receive-pack; Path for auth is *.
// Remaining is an exact positive consumption count, never an implicit forever.
type Fault struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Remaining int    `json:"remaining"`
	DelayMS   int    `json:"delay_ms,omitempty"`
	Status    int    `json:"status,omitempty"`
	Attempts  int    `json:"attempts"`
}

type Call struct {
	Method     string `json:"method"`
	Path       string `json:"path"`
	Unexpected bool   `json:"unexpected"`
}
type RepositoryState struct {
	Name string            `json:"name"`
	Keys []DeployKey       `json:"keys"`
	Refs map[string]string `json:"refs"`
}
type State struct {
	Repositories    []RepositoryState `json:"repositories"`
	Assets          []string          `json:"assets"`
	Calls           []Call            `json:"calls"`
	Faults          []Fault           `json:"faults"`
	UnexpectedCount int               `json:"unexpected_count"`
	HostKeys        []HostKey         `json:"host_keys"`
}

type HostKey struct {
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}

const segmentPattern = `[A-Za-z0-9_][A-Za-z0-9_.-]*`
const repositoryPattern = segmentPattern + "/" + segmentPattern

var repositoryPath = regexp.MustCompile(`^` + repositoryPattern + `$`)
var assetPath = regexp.MustCompile(`^/` + repositoryPattern + `/releases/download/` + segmentPattern + `/` + segmentPattern + `$`)

func validRepo(name string) bool {
	return repositoryPath.MatchString(name) && !strings.HasSuffix(name, ".git")
}
func validAsset(path string) bool { return assetPath.MatchString(path) }
func New(c Config) (*Fake, error) {
	if c.Now == nil {
		return nil, errors.New("clock is required")
	}
	if c.Delay == nil {
		c.Delay = delay
	}
	if !filepath.IsAbs(c.Root) {
		return nil, errors.New("fake root must be absolute")
	}
	if err := os.MkdirAll(c.Root, 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(c.Root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("fake root %s must be a real directory", c.Root)
	}
	signers := append([]ssh.Signer{}, c.HostKeys...)
	if len(signers) == 0 {
		var err error
		signers, err = generateHostKeys()
		if err != nil {
			return nil, err
		}
	}
	if c.Logger == nil {
		c.Logger = log.New(io.Discard, "", 0)
	}
	return &Fake{root: c.Root, now: c.Now, delay: c.Delay, signers: signers, logger: c.Logger, repos: map[string][]DeployKey{}, assets: map[string][]byte{}, nextID: 1}, nil
}

// ApplySeed accepts public keys only. Seed repository names are case insensitive.
func (f *Fake) ApplySeed(ctx context.Context, s Seed) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range s.Repositories {
		if err := f.seedRepository(ctx, r); err != nil {
			return err
		}
	}
	for _, line := range s.AuthorizedKeys {
		key, err := parseKey(line)
		if err != nil {
			return err
		}
		f.authorized = append(f.authorized, key)
	}
	for _, a := range s.Assets {
		if !validAsset(a.Path) {
			return errors.New("invalid asset path")
		}
		f.assets[normalizeHTTPPath(a.Path)] = slices.Clone(a.Bytes)
	}
	return nil
}
func (f *Fake) seedRepository(ctx context.Context, r RepositorySeed) error {
	if !validRepo(r.Name) {
		return errors.New("invalid repository name")
	}
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	if out, err := f.git(ctx, "check-ref-format", "refs/heads/"+r.DefaultBranch).CombinedOutput(); err != nil {
		return fmt.Errorf("invalid default branch: %s", out)
	}
	name := strings.ToLower(r.Name)
	if _, ok := f.repos[name]; ok {
		return nil
	}
	path := f.repoPath(name)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return fmt.Errorf("inspect repository %s: %w", path, err)
		}
		return fmt.Errorf("repository directory %s already exists", path)
	}
	if out, err := f.git(ctx, "init", "--bare", "--initial-branch="+r.DefaultBranch, path).CombinedOutput(); err != nil {
		return fmt.Errorf("initialize repository: %w: %s", err, out)
	}
	f.repos[name] = []DeployKey{}
	return nil
}
func parseKey(line string) (string, error) {
	k, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(options) > 0 || len(strings.TrimSpace(string(rest))) > 0 {
		return "", errors.New("expected one public key without options")
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))), nil
}
func (f *Fake) repoPath(name string) string { return filepath.Join(f.root, name+".git") }

// Snapshot includes refs from real Git and never includes credentials/private keys.
func (f *Fake) Snapshot(ctx context.Context) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := State{Repositories: []RepositoryState{}, Assets: []string{}, Calls: append([]Call{}, f.calls...), Faults: append([]Fault{}, f.faults...), UnexpectedCount: f.unexpected, HostKeys: []HostKey{}}
	for _, signer := range f.signers {
		s.HostKeys = append(s.HostKeys, HostKey{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), ssh.FingerprintSHA256(signer.PublicKey())})
	}
	for _, name := range slices.Sorted(maps.Keys(f.repos)) {
		out, err := f.git(ctx, "--git-dir="+f.repoPath(name), "for-each-ref", "--format=%(refname) %(objectname)").Output()
		if err != nil {
			return State{}, err
		}
		r := RepositoryState{Name: name, Keys: append([]DeployKey{}, f.repos[name]...), Refs: map[string]string{}}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if ref, sha, ok := strings.Cut(line, " "); ok {
				r.Refs[ref] = sha
			}
		}
		s.Repositories = append(s.Repositories, r)
	}
	s.Assets = slices.Sorted(maps.Keys(f.assets))
	return s, nil
}
func (f *Fake) record(method, path string, unexpected bool) {
	f.mu.Lock()
	f.calls = append(f.calls, Call{method, path, unexpected})
	if unexpected {
		f.unexpected++
	}
	f.mu.Unlock()
	if unexpected {
		f.logger.Printf("UNEXPECTED request: %s %s", method, path)
	}
}

// gitEnv prevents inherited configuration, hooks and Git environment overrides.
func (f *Fake) gitEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + f.root}
}
func (f *Fake) git(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "git", args...)
	c.Env = f.gitEnv()
	return c
}

func generateHostKeys() ([]ssh.Signer, error) {
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	keys := []ssh.Signer{}
	for _, private := range []any{ed, ec, rsaKey} {
		signer, err := ssh.NewSignerFromKey(private)
		if err != nil {
			return nil, err
		}
		keys = append(keys, signer)
	}
	return keys, nil
}
