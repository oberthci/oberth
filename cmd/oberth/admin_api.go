package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

func serverAdministration(options serveOptions, database *store.Store, cache *gitcache.Cache, gate func(context.Context) error) service.Administration {
	return service.Administration{
		Version:     version,
		TokenPolicy: options.uplinkTokenPolicy,
		EnsureUpstream: func(ctx context.Context, request api.UpstreamAddRequest) (api.UpstreamIdentity, error) {
			identities, err := newBaoIdentityStore(options.namespace)
			if err != nil {
				return api.UpstreamIdentity{}, err
			}
			keyName := filepath.Base(options.upstreamKey)
			if request.DedicatedKey {
				keyName = app.UpstreamKeyDataKey(keyName, request.Name)
			}
			bootstrap := app.UpstreamSSHBootstrap{
				Store: identities, Input: bytes.NewReader(nil), Output: io.Discard,
				ScanHostKeys: app.ScanSSHHostKeys, Probe: app.ProbeSSHAuthentication,
				Namespace: options.namespace, PrivateKeySecret: defaultUpstreamKeySecret, KnownHostsSecret: defaultUpstreamKnownHostsSecret,
				PrivateKeyPath: filepath.Join(filepath.Dir(options.upstreamKey), keyName), KnownHostsPath: options.knownHosts,
				PrivateKeyDataKey: keyName, PublicKeyDataKey: keyName + ".pub", KnownHostsDataKey: filepath.Base(options.knownHosts),
				MutationGate: func(ctx context.Context, _ string) error { return gate(ctx) },
				AutoYes:      true, NoWait: request.NoWait, ProbeWait: time.Second,
				ExpectedHostFingerprint: request.ExpectedHostFingerprint, Repin: request.Repin,
				CheckRepository: request.CheckRepository, RepositoryProbe: app.ProbeSSHRepository,
			}
			identity, err := bootstrap.Ensure(ctx, request.BaseURL)
			pending := errors.Is(err, app.ErrUpstreamBootstrapIncomplete) && ctx.Err() == nil
			if err != nil && !pending {
				return api.UpstreamIdentity{}, err
			}
			return api.UpstreamIdentity{PublicKey: string(identity.PublicKey), Fingerprint: identity.Fingerprint, Generated: identity.Generated, Pending: pending, RestartRequired: request.DedicatedKey}, nil
		},
		ImportUpstreamKey: func(ctx context.Context, request api.UpstreamKeyRequest) (api.UpstreamIdentity, error) {
			material := []byte(request.PrivateKey)
			defer clear(material)
			signer, err := ssh.ParsePrivateKey(material)
			if err != nil {
				return api.UpstreamIdentity{}, errors.New("invalid upstream private key")
			}
			public := ssh.MarshalAuthorizedKey(signer.PublicKey())
			keyName := "id_ed25519"
			if request.DedicatedKey {
				keyName = app.UpstreamKeyDataKey(keyName, request.Name)
			}
			identities, err := newBaoIdentityStore(options.namespace)
			if err != nil {
				return api.UpstreamIdentity{}, err
			}
			if err := gate(ctx); err != nil {
				return api.UpstreamIdentity{}, err
			}
			if err := identities.Save(ctx, defaultUpstreamKeySecret, map[string][]byte{keyName: material, keyName + ".pub": public}); err != nil {
				return api.UpstreamIdentity{}, err
			}
			return api.UpstreamIdentity{PublicKey: string(public), Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()), RestartRequired: request.DedicatedKey}, nil
		},
		CheckUpstream: func(ctx context.Context, upstream model.Upstream) (string, error) {
			repositories, err := database.ListRepositories(ctx)
			if err != nil {
				return "", err
			}
			repository := ""
			for _, candidate := range repositories {
				if candidate.UpstreamID == upstream.ID && candidate.ArchivedAt == nil {
					repository = candidate.Name
					break
				}
			}
			return upstreamAuthStatus(ctx, upstream, repository, options.upstreamKey, filepath.Dir(options.upstreamKey), options.knownHosts), nil
		},
		VerifyRepository: func(ctx context.Context, repository string) (any, error) {
			_, err := cache.LsRemoteHeads(ctx, repository)
			if err != nil {
				return nil, err
			}
			return map[string]any{"repo": repository, "verified": true}, nil
		},
		DatabaseBackup: func(ctx context.Context, output io.Writer, limit int64) error {
			configured := configuredStoreOptions(ctx)
			configured.BackupMaxBytes = limit
			_, err := store.WriteVerifiedBackup(ctx, options.database, store.LatestMigrationVersion(), configured, output)
			return err
		},
	}
}
