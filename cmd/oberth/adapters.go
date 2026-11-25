package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"golang.org/x/crypto/ssh"
)

type composition struct {
	settings       serverRuntime
	identity       *serverIdentity
	hostKey        ssh.Signer
	storeWriter    secretstore.Writer
	delivery       runtimeconfig.SecretDelivery
	views          apicontract.Views
	lifecycle      *lifecycle
	owners         *serverOwners
	config         serverConfig
	clock          serverClock
	store          *store.Store
	logs           *runlog.Store
	backup         *store.Backup
	schedules      *cronsync.Syncer
	scheduleConfig cronsync.Config
	bindings       apicontract.Bindings
	auth           api.Authenticator
	enroll         apicontract.Handler[model.InviteRedemption, model.InviteCertificate]
	streams        api.Streams
	accountBackup  api.AccountSnapshot
	source         http.Handler
	api            *api.Server
	health         *health
	runtime        api.Runtime
	installAdmin   func(context.Context, []byte, []byte) error
	ssh            func(context.Context, net.Listener) error
	cacheNotices   chan cacheNotice
	cacheGC        func(context.Context) error
	replayPending  func(context.Context) error
	watch          func(context.Context, func()) error
	diagnostic     io.Writer
}

// Each constructor fills its owner's ports; an absent constructor refuses startup.
// ServiceNew, WiringNew and NativeNew correspond to their owners' New constructors.
type constructors struct {
	CacheNew, NativeNew, WiringNew, ServiceNew, SourceNew func(context.Context, *composition) (Part, error)
}

func (c constructors) missing() []string {
	var names []string
	for name, missing := range map[string]bool{"gitcache.New": c.CacheNew == nil, "argojob.New/argowatch.New": c.NativeNew == nil, "app.New": c.WiringNew == nil, "service.New": c.ServiceNew == nil, "sourcedelivery.New/sshserver.New": c.SourceNew == nil} {
		if missing {
			names = append(names, name)
		}
	}
	return names
}

func openStorage(ctx context.Context, c *composition) ([]Part, error) {
	options := store.DefaultOptions(c.clock.Now)
	options.Protocol = c.config.Protocol
	db, err := openDatabase(ctx, c.config.DatabaseFile, options)
	if err != nil {
		return nil, err
	}
	c.store = db
	parts := []Part{{Name: "store", Shutdown: stopResources, Close: func(context.Context) error { return db.Close() }}}
	c.backup, err = store.NewBackup(c.config.DatabaseFile)
	if err != nil {
		return parts, err
	}
	c.logs, err = runlog.Open(ctx, filepath.Join(filepath.Dir(c.config.DatabaseFile), "logs"), c.config.Logs.RunLimitBytes)
	if err != nil {
		return parts, err
	}
	parts = append(parts, Part{Name: "logs", Shutdown: stopResources, Close: func(context.Context) error { return c.logs.Close() }})
	return parts, nil
}

func openDatabase(ctx context.Context, path string, options store.Options) (*store.Store, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		return store.Create(ctx, path, options)
	}
	return store.Open(ctx, path, options)
}

func newSchedules(c *composition) error {
	cfg := &c.scheduleConfig
	cfg.RetainAdmission = c.store.RetainSchedule
	cfg.PublishAdmission = c.store.PublishSchedule
	cfg.ForgetAdmission = c.store.ForgetSchedule
	cfg.PruneAdmissions = c.store.PruneSchedules
	cfg.Namespace = c.config.PipelineNamespace
	cfg.Now = c.clock.Now
	cfg.Limits = schedule.Limits{MaxEntries: c.config.Schedules.MaxEntries}
	cfg.ValidateNativeSchema = func(ctx context.Context, wf *wfv1.CronWorkflow, snapshots []admissioncontract.TemplateSnapshot) error {
		return argoworkflow.ValidateNativeSchema(ctx, &wfv1.Workflow{Spec: wf.Spec.WorkflowSpec}, admissioncontract.PrepareRequest{Templates: snapshots})
	}
	var err error
	c.schedules, err = cronsync.New(*cfg)
	return err
}

func newTransport(ctx context.Context, c *composition, diagnostic io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	registry, err := api.Builtin(c.bindings)
	if err != nil {
		return err
	}
	//nolint:contextcheck // The transport owns its observer context; BeginDrain cancels it before components stop.
	c.api, err = api.New(api.Config{Registry: registry, Auth: c.auth, Enroll: c.enroll, Streams: c.streams, AccountBackup: c.accountBackup, Source: c.source, Views: c.views, Runtime: c.runtime, Classify: publicerr.Classify,
		Ready: c.health.ready, Live: c.health.live, Metrics: func(ctx context.Context, _ apicontract.Caller) (string, error) { return c.health.metrics(ctx), nil }, FailureLog: slog.New(slog.NewTextHandler(diagnostic, nil))})
	return err
}

type storeAuth struct{ db *store.Store }

func (a storeAuth) Bearer(ctx context.Context, token string) (apicontract.Caller, error) {
	id := apicontract.SecretDigest(token)
	actor, err := a.db.Authenticate(ctx, 0, id)
	return apicontract.Caller{Actor: actor, CredentialID: id}, err
}
func (a storeAuth) Certificate(ctx context.Context, cert *x509.Certificate) (apicontract.Caller, error) {
	digest := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	id := hex.EncodeToString(digest[:])
	actor, err := a.db.AuthenticateCertificate(ctx, 0, base64.StdEncoding.EncodeToString(cert.RawSubjectPublicKeyInfo))
	return apicontract.Caller{Actor: actor, CredentialID: id}, err
}

func (c *composition) checkPorts() error {
	var names []string
	for name, missing := range map[string]bool{"service.InstallAdmin": c.installAdmin == nil, "source.SSH": c.ssh == nil, "source.Handler": c.source == nil, "service.Enroll": c.enroll == nil, "service.Streams": c.streams == nil, "gitcache.GC": c.cacheGC == nil, "gitcache.ReplayPending": c.replayPending == nil, "argowatch.Run": c.watch == nil, "service.Views.Counts": c.views.Counts.Handler == nil, "service.Views.Containers": c.views.Containers.Handler == nil, "service.Views.ContainerLog": c.views.ContainerLog.Handler == nil} {
		if missing {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(names) > 0 {
		return missingPart(names[0])
	}
	return nil
}
func watchPart(c *composition) Part {
	ready := make(chan struct{})
	var once sync.Once
	return Part{Name: "cluster", Run: func(ctx context.Context) error { return c.watch(ctx, func() { once.Do(func() { close(ready) }) }) }, Ready: func(ctx context.Context) error {
		select {
		case <-ready:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
}
