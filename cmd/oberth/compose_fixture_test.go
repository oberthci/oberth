package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wffake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktest "k8s.io/client-go/testing"
)

type composedFixture struct {
	*serverFixture
	git                     *gittest.Fixture
	argo                    *wffake.Clientset
	kube                    *kubefake.Clientset
	owners                  *serverOwners
	forge                   composedForge
	adminCertificate        tls.Certificate
	binary, identity, trust string
	ssh, sourceListener     net.Listener
	listed, allowList       chan struct{}
}

func composeFixture(t *testing.T, binary string) *composedFixture {
	t.Helper()
	f := &composedFixture{serverFixture: fixtureServer(t), git: gittest.New(t), argo: wffake.NewSimpleClientset(), kube: kubefake.NewClientset(), binary: binary, listed: make(chan struct{}), allowList: make(chan struct{})}
	rejectStreamingList := func(a ktest.Action) (bool, watch.Interface, error) {
		if a.(interface{ GetListOptions() meta.ListOptions }).GetListOptions().SendInitialEvents != nil {
			return true, nil, apierrors.NewBadRequest("fake cluster supports list then watch")
		}
		return false, nil, nil
	}
	f.kube.PrependWatchReactor("pods", rejectStreamingList)
	realNow := time.Now //nolint:forbidigo // Real Git children use sockets whose deadlines follow wall time.
	f.clock.Advance(realNow().Sub(f.clock.Now()))
	f.deps.clock.Now = realNow
	f.ca, f.key = authorityLifetime(t, f.clock.Now(), 365*24*time.Hour)
	for name, data := range map[string][]byte{"tls.crt": f.ca, "tls.key": f.key} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(f.config.Files.CACertificate), name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.config.DatabaseFile = filepath.Join(t.TempDir(), "fresh", "state.db")
	f.workflowWatch(t)
	stamp := func(a ktest.Action) (bool, runtime.Object, error) {
		object := a.(ktest.CreateAction).GetObject()
		m := object.(meta.Object)
		m.SetUID(types.UID("uid-" + m.GetName()))
		m.SetResourceVersion("1")
		m.SetCreationTimestamp(meta.NewTime(f.clock.Now()))
		return false, nil, nil
	}
	f.kube.PrependReactor("create", "serviceaccounts", stamp)
	releaseAccount, err := identityname.Name(identityname.Key{Repository: gittest.Repository}, identityname.Pipeline)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.kube.CoreV1().ServiceAccounts(f.config.PipelineNamespace).Create(t.Context(), &core.ServiceAccount{ObjectMeta: meta.ObjectMeta{Name: releaseAccount}}, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.argo.PrependReactor("create", "workflows", stamp)
	f.argo.PrependReactor("create", "cronworkflows", stamp)
	sshDir, forgeDir := t.TempDir(), t.TempDir()
	f.config.Files.SSHHostKey, _ = composeSSHKey(t, sshDir, "mounted-host-key")
	forge := composeForge(t, f.git)
	f.forge = forge
	f.identity, _ = composeSSHKey(t, t.TempDir(), client.UplinkPushKeyFile)
	f.config.Files.ForgeKey, _ = composeSSHKey(t, forgeDir, "mounted-forge-key")
	raw, _ := json.Marshal(serverRuntime{Routes: workflowpolicy.ReleaseRoutes{gittest.Repository: {{Pattern: "v*", File: ".oberth/release.yaml"}}}, GitBinary: f.git.Binary, SourceTimeoutSeconds: 15, AdmissionTimeoutSeconds: 10, SourceClientBurst: 30, DefaultBranch: "refs/heads/main", ImagePrefixes: []string{"oberth.ci/"}, IssueStaleSeconds: 86400, IssueActiveStaleSeconds: 7200})
	f.config.Runtime = raw
	platform := serverPlatform{cluster: func(context.Context) (clusterClients, error) {
		return clusterClients{Kube: f.kube, Argo: f.argo, REST: &rest.Config{Host: "https://cluster.oberth.ci"}}, nil
	}, executable: binary}
	builders := platform.constructors()
	builders.CacheNew = f.cacheBuilder(builders.CacheNew, forge)
	for name, target := range map[string]*func(context.Context, *composition) (Part, error){"cache": &builders.CacheNew, "native": &builders.NativeNew, "wiring": &builders.WiringNew, "service": &builders.ServiceNew, "source": &builders.SourceNew} {
		build := *target
		*target = func(ctx context.Context, c *composition) (Part, error) {
			f.trace.record("construct " + name)
			p, err := build(ctx, c)
			f.trace.record("constructed " + name)
			if recover := p.Recover; recover != nil {
				p.Recover = func(ctx context.Context) error { f.trace.record("recover " + name); return recover(ctx) }
			}
			return p, err
		}
	}
	f.deps.constructors = builders
	f.deps.listen = func(ctx context.Context, network, address string) (net.Listener, error) {
		if address == f.config.Listen.SSH {
			l, err := net.Listen(network, "127.0.0.1:0")
			f.ssh = l
			return l, err
		}
		if address == f.config.Listen.Source {
			l, err := net.Listen(network, "127.0.0.1:0")
			f.sourceListener = l
			return l, err
		}
		return f.listen(ctx, network, address)
	}
	f.trust = filepath.Join(t.TempDir(), "known_hosts")
	f.beforeCache = nil
	return f
}
func (f *composedFixture) cacheBuilder(cache func(context.Context, *composition) (Part, error), forge composedForge) func(context.Context, *composition) (Part, error) {
	return func(ctx context.Context, c *composition) (Part, error) {
		p, err := cache(ctx, c)
		f.owners = c.owners
		if err != nil {
			return p, err
		}
		if err = f.seed(ctx, "ssh://"+forge.address+"/team"); err != nil {
			return p, err
		}
		prepared, err := (serverAdministration{c.owners}).PrepareUpstream(ctx, apicontract.UpstreamAddArgs{Name: "origin", BaseURL: "ssh://" + forge.address + "/team", CheckRepo: "team/widget", ExpectedHostFingerprint: ssh.FingerprintSHA256(forge.key)})
		if err != nil {
			return p, err
		}
		if err = prepared.Persist(ctx); err != nil {
			return p, err
		}
		replay := c.replayPending
		c.replayPending = func(ctx context.Context) error { f.trace.record("replay pending"); return replay(ctx) }
		return p, err
	}
}

func composeSSHKey(t *testing.T, dir, name string) (string, ssh.PublicKey) {
	t.Helper()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err = client.WritePrivate(path, pem.EncodeToMemory(block)); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return path, signer.PublicKey()
}
func (f *composedFixture) seed(ctx context.Context, base string) error {
	db, err := store.Open(ctx, f.config.DatabaseFile, f.storeOptions())
	if err != nil {
		return err
	}
	defer db.Close()
	action := stateport.Action{Actor: "operator"}
	if err = db.RegisterUpstream(ctx, action, stateport.Upstream{Name: "origin", Org: "team", Kind: "ssh", BaseURL: base, KeyName: "shared"}); err != nil {
		return err
	}
	if _, err = db.RegisterRepository(ctx, action, gittest.Repository); err != nil {
		return err
	}
	raw, err := client.ReadPrivate(f.identity)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return err
	}
	if err = db.RegisterKey(ctx, action, stateport.Key{Identity: "agent", Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()), Kind: "agent", ParkOnPush: true, Scope: []string{"origin"}}); err != nil {
		return err
	}
	return nil
}
func (f *composedFixture) storeOptions() store.Options {
	options := store.DefaultOptions(f.clock.Now)
	options.Protocol = f.config.Protocol
	return options
}

// composeForge fakes only the forge: its SSH endpoint runs the real Git pack
// commands against an independent bare repository and joins them at teardown.
type composedForge struct {
	address  string
	key      ssh.PublicKey
	config   *atomic.Pointer[ssh.ServerConfig]
	sessions *atomic.Int64
}

func composeForge(t *testing.T, g *gittest.Fixture) composedForge {
	t.Helper()
	path, key := composeSSHKey(t, t.TempDir(), "host")
	raw, err := client.ReadPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	configuration := &atomic.Pointer[ssh.ServerConfig]{}
	configuration.Store(cfg)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	stopped := make(chan struct{})
	sessions := &atomic.Int64{}
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			sessions.Add(1)
			workers.Go(func() { composeForgeSession(ctx, conn, configuration.Load(), g) })
		}
	}()
	t.Cleanup(func() { cancel(); _ = listener.Close(); <-stopped; workers.Wait() })
	return composedForge{address: listener.Addr().String(), key: key, config: configuration, sessions: sessions}
}
func composeForgeSession(ctx context.Context, conn net.Conn, cfg *ssh.ServerConfig, g *gittest.Fixture) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	server, channels, requests, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer server.Close()
	go ssh.DiscardRequests(requests)
	for channel := range channels {
		composeForgeChannel(ctx, channel, g)
	}
}
func composeForgeChannel(ctx context.Context, channel ssh.NewChannel, g *gittest.Fixture) {
	stream, requests, err := channel.Accept()
	if err != nil {
		return
	}
	defer stream.Close()
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		var command struct{ Command string }
		if ssh.Unmarshal(request.Payload, &command) != nil {
			_ = request.Reply(false, nil)
			return
		}
		verb, bare := forgeRepository(g, command.Command)
		if verb == "" {
			_ = request.Reply(false, nil)
			return
		}
		_ = request.Reply(true, nil)
		native := exec.CommandContext(ctx, g.Binary, verb, bare)
		native.Stdin, native.Stdout, native.Stderr = stream, stream, stream.Stderr()
		native.WaitDelay = time.Second
		status := uint32(0)
		if native.Run() != nil {
			status = 1
		}
		_, _ = stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

// forgeRepository maps a forge command to its pack verb and bare repository:
// team/widget is the fixture's own; any other team/<name> is <root>/<name>.git.
func forgeRepository(g *gittest.Fixture, command string) (string, string) {
	verb, path, ok := strings.Cut(command, " ")
	verb = map[string]string{"git-upload-pack": "upload-pack", "git-receive-pack": "receive-pack"}[verb]
	name, found := strings.CutPrefix(path, "'/team/")
	name, suffixed := strings.CutSuffix(name, ".git'")
	if !ok || !found || !suffixed || name == "" || strings.ContainsAny(name, "/'.") {
		return "", ""
	}
	if name == "widget" {
		return verb, g.Bare
	}
	return verb, filepath.Join(g.Root, name+".git")
}

func (f *composedFixture) workflowWatch(t *testing.T) {
	t.Helper()
	var once sync.Once
	var workers sync.WaitGroup
	f.argo.PrependWatchReactor("workflows", func(a ktest.Action) (bool, watch.Interface, error) {
		options := a.(interface{ GetListOptions() meta.ListOptions }).GetListOptions()
		if options.SendInitialEvents == nil {
			return false, nil, nil
		}
		tracker, err := f.argo.Tracker().Watch(a.GetResource(), a.GetNamespace(), options)
		if err != nil {
			return true, nil, err
		}
		once.Do(func() { close(f.listed) })
		workers.Go(func() {
			select {
			case <-f.allowList:
				tracker.(*watch.RaceFreeFakeWatcher).Action(watch.Bookmark, &wf.Workflow{ObjectMeta: meta.ObjectMeta{ResourceVersion: "1", Annotations: map[string]string{meta.InitialEventsAnnotationKey: "true"}}})
			case <-t.Context().Done():
			}
		})
		return true, tracker, nil
	})
	t.Cleanup(workers.Wait)
}
