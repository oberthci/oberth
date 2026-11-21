package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
)

type serverDependencies struct {
	constructors constructors
	clock        serverClock
	listen       func(context.Context, string, string) (net.Listener, error)
	timeout      func(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

func realServerDependencies(now func() time.Time) (serverDependencies, error) {
	executable, err := os.Executable()
	if err != nil {
		return serverDependencies{}, serverProblem("config", "Cannot locate the server executable.", err)
	}
	platform := serverPlatform{cluster: inClusterClients, executable: executable}
	return serverDependencies{constructors: platform.constructors(), clock: serverClock{now, realTicker}, listen: (&net.ListenConfig{}).Listen, timeout: context.WithTimeout}, nil
}
func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}
func transportRuntime(clock serverClock) api.Runtime {
	return api.Runtime{Now: clock.Now, Ticker: clock.Ticker, Nonce: func() string {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return ""
		}
		return hex.EncodeToString(b[:])
	}}
}
func (a application) serve(ctx context.Context, args []string) int {
	if len(args) == 1 && args[0] == "--help" {
		return a.serveHelp()
	}
	path := ""
	if len(args) == 1 && strings.HasPrefix(args[0], "--config=") {
		path = strings.TrimPrefix(args[0], "--config=")
	}
	if len(args) == 2 && args[0] == "--config" {
		path = args[1]
	}
	if path == "" {
		return a.usage([]string{"serve requires --config <file> or --config=<file>"})
	}
	cfg, err := readConfig(path)
	if err == nil {
		deps := a.server
		if deps == nil {
			d, dependencyErr := realServerDependencies(a.config.Time)
			if dependencyErr != nil {
				return a.failedServer(dependencyErr)
			}
			deps = &d
		}
		err = serve(ctx, cfg, *deps, a.diagnostic)
	}
	if err != nil {
		return a.failedServer(err)
	}
	return 0
}
func (a application) failedServer(err error) int {
	return a.failedDetail(err, true)
}

func serve(ctx context.Context, cfg serverConfig, deps serverDependencies, diagnostic io.Writer) (resultErr error) {
	if missing := deps.constructors.missing(); len(missing) > 0 {
		slices.Sort(missing)
		return missingPart(missing[0])
	}
	inputs, err := resolveStartup(cfg, deps)
	if err != nil {
		return err
	}
	if deps.timeout == nil {
		deps.timeout = context.WithTimeout
	}
	life := newLifecycle(ctx, nil)
	life.timeout, life.diagnostic = deps.timeout, diagnostic
	stopStartup := context.AfterFunc(ctx, life.abortStart)
	defer stopStartup()
	h := &health{clock: deps.clock, validLeaf: inputs.identity.valid, leafExpiry: inputs.identity.expiry, completed: deps.clock.Now(), readySignal: make(chan struct{}), sampleReady: make(chan struct{})}
	c := &composition{lifecycle: life, config: cfg, clock: deps.clock, runtime: transportRuntime(deps.clock), health: h, diagnostic: diagnostic, settings: inputs.settings, identity: inputs.identity, hostKey: inputs.host}
	s := newServing(c, life, deps)
	defer func() { resultErr = errors.Join(resultErr, s.close(ctx)) }()
	if err = s.construct(ctx, inputs.admin); err != nil {
		return err
	}
	if err = s.start(ctx); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	h.setStarted(true)
	if _, err = fmt.Fprintln(diagnostic, "ready: https "+cfg.Listen.HTTPS+", ssh "+cfg.Listen.SSH+", source "+cfg.Listen.Source); err != nil {
		return err
	}
	return s.wait(ctx)
}
func readMounts(cfg serverConfig, deps serverDependencies) (*serverIdentity, administrator, error) {
	u, _ := url.Parse(cfg.Public.URL)
	names := []string{u.Hostname(), "oberth", "oberth." + cfg.Namespace + ".svc", "oberth." + cfg.Namespace + ".svc.cluster.local"}
	identity, err := mountedIdentity(cfg.Files.CACertificate, cfg.Files.CAPrivateKey, names, deps.clock.Now)
	if err != nil {
		return nil, administrator{}, err
	}
	keys, err := readAdmin(cfg.Files.AdminPublicKey, cfg.Files.AdminPushKey)
	return identity, keys, err
}
func (a application) serveHelp() int {
	for _, line := range []string{"oberth serve --config <file>", commandCatalogue()["serve"].Summary, "--config (file): Strict server configuration.", "--help, -h: Show this help."} {
		if err := writeWrapped(a.out, line); err != nil {
			return 1
		}
	}
	return 0
}

func (s *serving) construct(ctx context.Context, admin administrator) error {
	c, life := s.c, s.life
	var err error
	c.storeWriter, c.delivery, err = serverStoreWriter(ctx, c.config)
	if err != nil {
		return err
	}
	life.add(Part{Name: "secret store", Shutdown: stopResources, Close: func(context.Context) error { c.storeWriter.Client.Close(); return nil }})
	if err = s.listen(ctx, c.identity); err != nil {
		return err
	}
	life.add(Part{Name: "administrator", Recover: func(ctx context.Context) error { return admin.install(ctx, c.installAdmin) }})
	if err = assemble(ctx, c, life, s.deps.constructors); err != nil {
		return err
	}
	s.publish()
	life.add(sshPart(c, s.deps.listen))
	life.onFailure = func() { c.health.setStarted(false) }
	s.starting = make(chan error, 1)
	go func() { s.starting <- life.start() }()

	return nil
}
