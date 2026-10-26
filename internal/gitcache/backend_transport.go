package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	hostkeys "github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Private schemes avoid changing go-git's process-wide default transports.
// In particular its default file client executes git-upload/receive-pack.
func init() {
	client.InstallProtocol("oberth-file", goFileTransport{})
	client.InstallProtocol("oberth-ssh", strictSSHTransport{})
}

type SSHIdentity struct {
	Host           string
	PrivateKeyPath string
}
type SSHConfig struct {
	PrivateKeyPath string
	KnownHostsPath string
	Identities     []SSHIdentity
}

type strictSSHAuth struct {
	ctx    context.Context
	config *ssh.ClientConfig
}

func (a *strictSSHAuth) Name() string   { return "oberth-pinned-key" }
func (a *strictSSHAuth) String() string { return a.Name() }

func (b *goGitBackend) remoteAuth(ctx context.Context, remote string) (string, transport.AuthMethod, error) {
	if err := ValidateUpstream(remote); err != nil {
		return "", nil, err
	}
	if !strings.Contains(remote, "://") {
		return "oberth-file://" + remote, nil, nil
	}
	parsed, err := url.Parse(remote)
	if err != nil {
		return "", nil, err
	}
	if parsed.Scheme != "ssh" {
		return remote, nil, nil
	}
	settings := b.cache.ssh
	path := settings.PrivateKeyPath
	for _, entry := range settings.Identities {
		if strings.EqualFold(entry.Host, parsed.Hostname()) {
			path = entry.PrivateKeyPath
			break
		}
	}
	if path == "" || settings.KnownHostsPath == "" {
		return "", nil, errors.New("SSH private key and pinned known_hosts are required; ambient SSH configuration is not used")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1<<20 || info.Mode().Perm()&0o037 != 0 {
		return "", nil, errors.New("SSH private key must be a bounded owner/group-readable regular file, never group-writable or world-accessible")
	}
	// #nosec G304 -- bounded administrator-owned SSH private-key path, never a repository path.
	body, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	signer, err := ssh.ParsePrivateKey(body)
	clear(body)
	if err != nil {
		return "", nil, fmt.Errorf("parse upstream SSH identity: %w", err)
	}
	info, err = os.Stat(settings.KnownHostsPath)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1<<20 {
		return "", nil, errors.New("known_hosts must be a nonempty regular file of at most 1 MiB")
	}
	callback, err := knownhosts.New(settings.KnownHostsPath)
	if err != nil {
		return "", nil, fmt.Errorf("load pinned known_hosts: %w", err)
	}
	database, err := hostkeys.NewDB(settings.KnownHostsPath)
	if err != nil {
		return "", nil, err
	}
	port := parsed.Port()
	if port == "" {
		port = "22"
	}
	algorithms := database.HostKeyAlgorithms(net.JoinHostPort(parsed.Hostname(), port))
	if len(algorithms) == 0 {
		return "", nil, errors.New("SSH endpoint has no pinned host key")
	}
	user := "git"
	if parsed.User != nil && parsed.User.Username() != "" {
		user = parsed.User.Username()
	}
	parsed.Scheme = "oberth-ssh"
	return parsed.String(), &strictSSHAuth{ctx: ctx, config: &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: callback, HostKeyAlgorithms: algorithms}}, nil
}

func remoteURL(r *git.Repository, name string) (string, error) {
	remote, err := r.Remote(name)
	if err == nil {
		if len(remote.Config().URLs) != 1 {
			return "", errors.New("remote must have exactly one URL")
		}
		return remote.Config().URLs[0], nil
	}
	if errors.Is(err, git.ErrRemoteNotFound) {
		return name, nil
	}
	return "", err
}
func positionals(a []string) []string {
	var result []string
	for _, v := range a {
		if !strings.HasPrefix(v, "-") {
			result = append(result, v)
		}
	}
	return result
}
func (b *goGitBackend) fetch(ctx context.Context, r *git.Repository, a []string) error {
	args := positionals(a)
	if len(args) == 0 {
		return errors.New("missing fetch remote")
	}
	original, err := remoteURL(r, args[0])
	if err != nil {
		return err
	}
	remote, auth, err := b.remoteAuth(ctx, original)
	if err != nil {
		return err
	}
	refs := []config.RefSpec{}
	for _, value := range args[1:] {
		if ValidateSHA(value) == nil {
			value += ":refs/oberth/workspace-fetch"
		}
		refs = append(refs, config.RefSpec(value))
	}
	// Construct a transient remote; persist only administrator URLs in config.
	originalStore, ok := r.Storer.(*atomicReferenceStore)
	if !ok {
		return errors.New("atomic reference store required")
	}
	known, err := goKnownRoots(r)
	if err != nil {
		return err
	}
	staged := &stagedReferenceStore{atomicReferenceStore: originalStore, updates: map[plumbing.ReferenceName]*plumbing.Reference{}, removed: map[plumbing.ReferenceName]bool{}}
	r.Storer = staged
	defer func() { r.Storer = originalStore }()
	upstream := git.NewRemote(r.Storer, &config.RemoteConfig{Name: args[0], URLs: []string{remote}})
	if !strings.Contains(original, "://") {
		err = fetchLocalObjects(ctx, r, original, refs, contains(a, "--prune"))
	} else {
		err = upstream.FetchContext(ctx, &git.FetchOptions{RemoteName: args[0], RemoteURL: remote, Auth: auth, RefSpecs: refs, Force: contains(a, "--force"), Prune: contains(a, "--prune"), Tags: git.NoTags})
	}
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		for _, ref := range refs {
			if !ref.IsWildcard() {
				return err
			}
		}
	}
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return err
	}
	roots := []plumbing.Hash{}
	for _, ref := range staged.updates {
		if ref.Type() == plumbing.HashReference {
			roots = append(roots, ref.Hash())
		}
	}
	if err := validateGoGraph(ctx, r, roots, known); err != nil {
		return err
	}
	if err := staged.commit(); err != nil {
		return err
	}
	if len(args) == 2 && ValidateSHA(args[1]) == nil {
		return originalStore.SetReference(plumbing.NewHashReference("FETCH_HEAD", plumbing.NewHash(args[1])))
	}
	return nil
}
func (b *goGitBackend) push(ctx context.Context, r *git.Repository, a []string) error {
	args := positionals(a)
	if len(args) < 2 {
		return errors.New("missing push refspec")
	}
	original, err := remoteURL(r, args[0])
	if err != nil {
		return err
	}
	remote, auth, err := b.remoteAuth(ctx, original)
	if err != nil {
		return err
	}
	refs := []config.RefSpec{}
	for _, value := range args[1:] {
		source, destination, ok := strings.Cut(value, ":")
		if ok && ValidateSHA(source) == nil {
			name := plumbing.ReferenceName("refs/oberth/outgoing/" + source)
			if err := r.Storer.SetReference(plumbing.NewHashReference(name, plumbing.NewHash(source))); err != nil {
				return err
			}
			defer func() {
				store, ok := r.Storer.(*atomicReferenceStore)
				if ok {
					_ = store.deleteReference(name, plumbing.NewHashReference(name, plumbing.NewHash(source)))
				}
			}()
			value = name.String() + ":" + destination
		}
		refs = append(refs, config.RefSpec(value))
	}
	upstream := git.NewRemote(r.Storer, &config.RemoteConfig{Name: args[0], URLs: []string{remote}})
	err = upstream.PushContext(ctx, &git.PushOptions{RemoteName: args[0], RemoteURL: remote, Auth: auth, RefSpecs: refs, Force: contains(a, "--force")})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}
func (b *goGitBackend) clone(ctx context.Context, a []string) (result error) {
	args := positionals(a)
	if len(args) != 2 {
		return errors.New("invalid workspace clone")
	}
	source, dest := args[0], args[1]
	defer func() {
		if result != nil {
			_ = os.RemoveAll(dest)
		}
	}()
	r, err := git.PlainInit(dest, false)
	if err != nil {
		return err
	}
	wrapGoReferences(r, dest+"/.git")
	defer func() {
		if closer, ok := r.Storer.(io.Closer); ok {
			result = errors.Join(result, closer.Close())
		}
	}()
	if _, err := r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{source}}); err != nil {
		return err
	}
	return b.fetch(ctx, r, []string{"origin", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"})
}
func (b *goGitBackend) lsRemote(ctx context.Context, dir string, a []string, out io.Writer) error {
	args := positionals(a)
	if len(args) == 0 {
		return errors.New("missing ls-remote remote")
	}
	original := args[0]
	if dir != "" {
		r, closeStorage, err := openGoRepository(dir)
		if err != nil {
			return err
		}
		defer closeStorage()
		original, err = remoteURL(r, original)
		if err != nil {
			return err
		}
	}
	remote, auth, err := b.remoteAuth(ctx, original)
	if err != nil {
		return err
	}
	upstream := git.NewRemote(nil, &config.RemoteConfig{Name: "probe", URLs: []string{remote}})
	refs, err := upstream.ListContext(ctx, &git.ListOptions{Auth: auth, PeelingOption: git.AppendPeeled})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil
	}
	if err != nil {
		return err
	}
	sortGoRefs(refs)
	for _, ref := range refs {
		name := ref.Name().String()
		if contains(a, "--heads") && !strings.HasPrefix(name, "refs/heads/") {
			continue
		}
		if contains(a, "--tags") && !strings.HasPrefix(name, "refs/tags/") {
			continue
		}
		if contains(a, "--refs") && (!strings.HasPrefix(name, "refs/") || strings.HasSuffix(name, "^{}")) {
			continue
		}
		if len(args) > 1 && name != args[1] {
			continue
		}
		if ref.Type() == plumbing.SymbolicReference {
			if contains(a, "--symref") {
				if _, err := fmt.Fprintf(out, "ref: %s\t%s\n", ref.Target(), name); err != nil {
					return err
				}
			}
			for _, target := range refs {
				if target.Name() == ref.Target() {
					if _, err := fmt.Fprintf(out, "%s\t%s\n", target.Hash(), name); err != nil {
						return err
					}
					break
				}
			}
			continue
		}
		if _, err := fmt.Fprintf(out, "%s\t%s\n", ref.Hash(), name); err != nil {
			return err
		}
	}
	return nil
}
func sortGoRefs(refs []*plumbing.Reference) {
	slices.SortFunc(refs, func(a, b *plumbing.Reference) int { return strings.Compare(a.Name().String(), b.Name().String()) })
}

// Strict SSH transport bounds connection establishment AND the handshake by
// the operation context, rather than only giving the TCP dial a timeout.
type strictSSHTransport struct{}

func (strictSSHTransport) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	return newStrictSSHSession(ep, auth, "git-upload-pack")
}
func (strictSSHTransport) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	return newStrictSSHSession(ep, auth, "git-receive-pack")
}
func sshAddress(ep *transport.Endpoint) string {
	port := ep.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(ep.Host, strconv.Itoa(port))
}
