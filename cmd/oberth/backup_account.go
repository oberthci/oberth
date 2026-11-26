package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/kubeconfig"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type accountDatabase struct {
	client    kubernetes.Interface
	config    installer.BackupJobConfig
	http      *http.Client
	inspector stateport.Backup
}

func (s accountDatabase) Snapshot(ctx context.Context, options stateport.Options, out io.Writer) (stateport.BackupReport, error) {
	jwt, err := installer.BackupProjection(ctx, s.client, s.config.Installation.Namespace, apicontract.BackupSnapshotAudience)
	if err != nil {
		return stateport.BackupReport{}, backupAccountProblem()
	}
	defer clear(jwt)
	response, err := s.databaseResponse(ctx, jwt)
	if err != nil {
		return stateport.BackupReport{}, backupAccountProblem()
	}
	defer response.Body.Close()
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/octet-stream" || response.StatusCode != http.StatusOK {
		return stateport.BackupReport{}, backupAccountProblem()
	}
	return s.inspector.Inspect(ctx, options, io.TeeReader(response.Body, out))
}

func (a application) backupInCluster(ctx context.Context, r installationRequest, out io.Writer) error {
	if r.text("namespace") == "" || r.text("profile") != "" || r.enabled("dry-run") || r.enabled("database-only") {
		return backupAccountProblem()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	access, err := rest.InClusterConfig()
	if err != nil {
		return backupAccountProblem()
	}
	client, err := kubernetes.NewForConfig(access)
	if err != nil {
		return backupAccountProblem()
	}
	config, err := installer.ReadBackupJob(ctx, client, r.text("namespace"))
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "oberth-backup-check-*")
	if err != nil {
		return backupAccountProblem()
	}
	defer os.RemoveAll(dir)
	inspector, err := store.NewBackup(filepath.Join(dir, "inspect.sqlite"))
	if err != nil {
		return err
	}
	httpClient, err := accountBackupHTTP(config.ServerCA)
	if err != nil {
		return err
	}
	defer httpClient.CloseIdleConnections()
	engine := accountBackupEngine(client, access, config)
	keep, _ := strconv.Atoi(r.text("keep"))
	archive, err := engine.BackupAccount(ctx, config, installer.BackupConfig{Source: accountDatabase{client: client, config: config, http: httpClient, inspector: inspector},
		Path: r.text("out"), Directory: r.text("directory"), Keep: keep, Now: a.config.Time()})
	return (installationSession{app: a, request: r}).backupResult(out, archive, err)
}

func accountBackupHTTP(ca []byte) (*http.Client, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, backupAccountProblem()
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func accountBackupEngine(client kubernetes.Interface, access *rest.Config, config installer.BackupJobConfig) *installer.Engine {
	engine := &installer.Engine{Cluster: kubeconfig.Target{Client: client, Config: access}}
	engine.OpenStore = func(_ context.Context, cfg secretstore.Config) (*secretstore.Client, func(), error) {
		if cfg.ServerNamespace != config.Installation.Namespace {
			return nil, nil, backupAccountProblem()
		}
		name := "oberth-store." + cfg.ServerNamespace + ".svc"
		store, err := secretstore.New(secretstore.Connection{URL: "https://" + name + ":8200", ServerName: name, CA: config.ServerCA})
		return store, func() {}, err
	}
	return engine
}

func backupAccountProblem() error {
	return installationProblem("Backup account access is incomplete.", "Use the scoped backup Job in the server namespace; keep operator profiles and decryption keys off the cluster.")
}
