package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"golang.org/x/crypto/ssh"
)

type serverAdministration struct{ owner *serverOwners }

func (a serverAdministration) PrepareUpstream(ctx context.Context, q apicontract.UpstreamAddArgs) (service.PreparedUpstream, error) {
	var prepared service.PreparedUpstream
	if err := reponame.ValidateUpstreamName(q.Name); err != nil {
		return prepared, err
	}
	u, err := url.Parse(q.BaseURL)
	if err != nil || u.Hostname() == "" {
		return prepared, serverProblem("config", "upstream endpoint is invalid", err)
	}
	name := "shared"
	if q.DedicatedKey {
		name = q.Name
	}
	org, err := upstreamOrganization(q, u)
	if err != nil {
		return prepared, err
	}
	trust, persist, err := a.preparePins(ctx, u, q.ExpectedHostFingerprint, q.Repin)
	if err != nil {
		return prepared, err
	}
	prepared.Binding = stateport.Upstream{Name: q.Name, Org: org, BaseURL: q.BaseURL, Kind: "ssh", KeyName: name}
	prepared.Offered, prepared.Replaced, prepared.Persist = trust.Offered, trust.Replaced, persist
	return prepared, nil
}
func (a serverAdministration) PersistKey(ctx context.Context, q apicontract.UpstreamProvideKeyArgs) (apicontract.Upstream, error) {
	u, err := a.owner.c.store.Upstream(ctx, q.Name)
	if err != nil {
		return apicontract.Upstream{}, err
	}
	if _, err = ssh.ParsePrivateKey([]byte(q.PrivateKey)); err != nil {
		return apicontract.Upstream{}, err
	}
	dir := a.identityDirectory(u.KeyName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return apicontract.Upstream{}, err
	}
	path := filepath.Join(dir, "id_ed25519")
	if err = writeServerState(path, []byte(q.PrivateKey)); err != nil {
		return apicontract.Upstream{}, err
	}
	return a.ProbeUpstream(ctx, q.Name, "")
}
func (a serverAdministration) identityDirectory(name string) string {
	return filepath.Join(filepath.Dir(a.owner.c.config.DatabaseFile), "identities", name)
}
func (a serverAdministration) keyPath(name string) (string, error) {
	dir := a.identityDirectory(name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "id_ed25519")
	if _, err := os.Lstat(path); err == nil {
		return path, checkServerState(path, false)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if name == "shared" {
		return a.owner.c.config.Files.ForgeKey, nil
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	defer clear(key)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		return "", err
	}
	if err = writeServerState(path, pem.EncodeToMemory(block)); err != nil {
		return "", err
	}
	return path, nil
}
func (a serverAdministration) configure(ctx context.Context, u stateport.Upstream) (string, error) {
	key, err := a.keyPath(u.KeyName)
	if err != nil {
		return "", err
	}
	endpoint, err := url.Parse(u.BaseURL)
	if err != nil {
		return "", err
	}
	port := endpoint.Port()
	if port == "" {
		port = "22"
	}
	dir := a.identityDirectory(u.KeyName)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "ssh-"+u.Name+".config")
	data := sshConfiguration(sshConnection{Host: endpoint.Hostname(), Port: port, User: reponame.ForgeUser(endpoint), Key: key, Hosts: a.pinsPath()})
	return path, errors.Join(ctx.Err(), writeServerState(path, data))
}
func (a serverAdministration) PublicIdentity(ctx context.Context, name string) (apicontract.PublicIdentity, error) {
	if err := reponame.ValidateUpstreamName(name); err != nil {
		return apicontract.PublicIdentity{}, err
	}
	path, err := a.keyPath(name)
	if err != nil {
		return apicontract.PublicIdentity{}, err
	}
	var raw []byte
	if path == a.owner.c.config.Files.ForgeKey {
		raw, err = secretstore.ReadMountedFile(path, 64<<10, "files.forge_key")
	} else {
		raw, err = readServerState(path)
	}
	if err != nil {
		return apicontract.PublicIdentity{}, err
	}
	defer clear(raw)
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return apicontract.PublicIdentity{}, err
	}
	return apicontract.PublicIdentity{Name: name, PublicKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey()))}, ctx.Err()
}
func (a serverAdministration) Status(ctx context.Context, action string) (apicontract.SystemStatus, error) {
	err := a.owner.c.health.ready(ctx)
	result := apicontract.SystemStatus{Parts: a.statusParts(ctx), Ready: err == nil, DisplayReason: "ready", Version: binaryVersion()}
	if err != nil {
		result.DisplayReason = "not ready"
	}
	for _, p := range result.Parts {
		if !p.Ready {
			result.Ready, result.DisplayReason = false, "not ready"
		}
	}
	if action == "admin_idle" {
		idle, err := a.owner.c.store.Idle(ctx)
		if err != nil {
			return result, err
		}
		if !idle {
			result.DisplayReason = "runs are active"
		}
	}
	return result, nil
}
func (a serverAdministration) ReceiveOutbox(ctx context.Context, action stateport.Action, scope []string, q apicontract.ReceiveOutboxArgs) (apicontract.OutboxResult, error) {
	records, err := a.owner.cache.PendingReceives(ctx)
	if err != nil {
		return apicontract.OutboxResult{}, err
	}
	out := apicontract.OutboxResult{Records: []apicontract.OutboxRecord{}}
	for _, pending := range records {
		if scope != nil && !slices.Contains(scope, pending.Event.Repository) || q.Repo != "" && q.Repo != pending.Event.Repository {
			continue
		}
		if q.ID != "" && pending.Event.ID != q.ID {
			continue
		}
		if err := a.repairReceive(ctx, action, q, pending); err != nil {
			return out, err
		}
		reason := pending.Problem
		if reason == "" {
			reason = "receive pending"
		}
		out.Records = append(out.Records, apicontract.OutboxRecord{ID: pending.Event.ID, Repo: pending.Event.Repository, State: pending.State, DisplayReason: reason})
	}
	return out, nil
}

type sshConnection struct{ Host, Port, User, Key, Hosts string }

func sshConfiguration(c sshConnection) []byte {
	return fmt.Appendf(nil, "Host %s\nHostName %s\nUser %s\nPort %s\nIdentityFile %q\nUserKnownHostsFile %q\nIdentitiesOnly yes\nBatchMode yes\nStrictHostKeyChecking yes\nGlobalKnownHostsFile /dev/null\n", c.Host, c.Host, c.User, c.Port, c.Key, c.Hosts)
}

func (a serverAdministration) repairReceive(ctx context.Context, action stateport.Action, q apicontract.ReceiveOutboxArgs, pending gitcache.Reservation) error {
	if q.Action == "list" || q.Action == "inspect" {
		return nil
	}
	if q.Action != "retry" && q.Action != "abandon" {
		return publicerr.New(publicerr.InvalidInput, "Unknown receive repair action.", "Use list, inspect, retry or abandon.", nil)
	}
	if err := a.owner.recordReceiveRepair(ctx, action, q.Action, pending.Event); err != nil {
		return err
	}
	if q.Action == "retry" {
		return a.owner.cache.RetryReceive(ctx, pending.Event.Repository, a.owner.deliver)
	}
	return a.owner.cache.AbandonReceive(ctx, pending.Event.Repository, pending.Event.ID)
}

type forgeAddress string

func (a forgeAddress) Network() string { return "tcp" }
func (a forgeAddress) String() string  { return string(a) }
