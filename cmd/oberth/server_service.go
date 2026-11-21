package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/clientcert/issuer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

func (o *serverOwners) servicePart(ctx context.Context) (Part, error) {
	o.c.identity.mu.Lock()
	cert, key := bytes.Clone(o.c.identity.certBytes), bytes.Clone(o.c.identity.keyBytes)
	o.c.identity.mu.Unlock()
	defer clear(key)
	authority, err := issuer.Load(cert, key, rand.Reader, o.c.clock.Now)
	if err != nil {
		return Part{}, err
	}
	parsed, err := clientcert.ParseCertificate(cert)
	if err != nil {
		return Part{}, err
	}
	db := o.c.store
	people := service.People{IssueWindows: o.settings.issueWindows(), StateMetadata: db, SecretWriter: o.c.storeWriter, ReleaseRoutes: o.settings.Routes, Issues: db, Enrollment: serverPeopleStore{db, o}, Grants: db, Publication: authorityPublication{o}, Cluster: clusterPolicy{o}, Administration: serverAdministration{o}, Admin: serverPeopleStore{db, o}, Issuer: authority, Backup: o.c.backup, BackupOptions: stateport.Options{Protocol: o.c.config.Protocol, Schema: store.Schema}, CAPin: clientcert.Pin(parsed), PublicURL: o.c.config.Public.URL, SSHURL: advertisedSSH(o.c.config), Random: rand.Reader, Schedules: serverSchedules{o}}
	o.service, err = service.New(ctx, service.Config{People: people, Publications: db, Promotions: db, MovingGit: o.cache, HoldStore: db, Holds: heldRuns{o.native}, Runs: db, Authority: currentAuthority{o}, Catalog: db, Events: db, Git: o, Source: serviceSource{o.wiring, o.settings.DefaultBranch}, Exec: o, Starter: o.wiring, Logs: serverLogs{o}, Now: o.c.clock.Now, NewID: rand.Text, AfterFunc: func(d time.Duration, f func()) service.Timer { return time.AfterFunc(d, f) }, OnError: o.notice})
	if err != nil {
		return Part{}, err
	}
	o.c.bindings = o.service.Bindings()
	o.c.views = o.service.Views()
	o.c.enroll = enrollment{o}
	o.c.streams = serverStreams{o}
	o.c.installAdmin = o.service.InstallAdmin
	return Part{Name: "service", Shutdown: stopService, Recover: o.service.RecoverPublications, Close: o.service.Shutdown}, nil
}

type enrollment struct{ owner *serverOwners }

func (e enrollment) Prepare(ctx context.Context, _ apicontract.Caller, q model.InviteRedemption) (apicontract.Call[model.InviteCertificate], error) {
	id, err := e.owner.c.store.InvitationID(ctx, apicontract.SecretDigest(q.Secret))
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (model.InviteCertificate, error) { return e.owner.service.Redeem(ctx, id, q) }, nil
}

func advertisedSSH(config serverConfig) string {
	address, _ := url.Parse(config.Public.URL)
	return (&url.URL{Scheme: "ssh", User: url.User("git"), Host: net.JoinHostPort(address.Hostname(), strconv.Itoa(config.Public.SSHPort))}).String()
}

// The protected branch name is configuration, independent of forge availability.
type serviceSource struct {
	*app.Submitter
	defaultBranch string
}

func (s serviceSource) DefaultBranch(ctx context.Context, _ string) (string, error) {
	return s.defaultBranch, ctx.Err()
}
