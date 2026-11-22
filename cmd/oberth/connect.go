package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/strictjson"
)

func invite(s *flowSession, args []string) error {
	f := flowFlags("invite", s.app.home())
	rest, err := parseFlow(f, args)
	identity := flagValue[string](f, "identity")
	out := flagValue[string](f, "out")
	expires := flagValue[string](f, "expires")
	if err != nil {
		return err
	}
	if !validFlowArgs("invite", rest) {
		return usageProblems{"invite requires an upstream"}
	}
	if err = s.connect(); err != nil {
		return err
	}
	hosts, err := s.profileHostKeys()
	if err != nil {
		return err
	}
	if *out == "" {
		root := s.app.base
		if s.resolved.ClientKey != "" {
			root = filepath.Dir(s.resolved.ClientKey)
		} else if s.resolved.TokenFile != "" {
			root = filepath.Dir(s.resolved.TokenFile)
		}
		dir := filepath.Join(root, "invitations")
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		*out = filepath.Join(dir, rand.Text()+".json")
	}
	path, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	result, err := s.remote.Invite(s.ctx, apicontract.UplinkInviteArgs{Identity: *identity, Scope: []string{rest[0]}, Expires: *expires, ServerURL: s.resolved.URL}, path, hosts)
	if err != nil {
		return err
	}
	s.last, _ = json.Marshal(result)
	return s.fact("invitation: %s\nrecipient runs: oberth join %s", path, shellArgument(path))
}
func join(s *flowSession, args []string) error {
	if !validFlowArgs("join", args) {
		return usageProblems{"join requires one invitation file"}
	}
	data, err := client.ReadPrivate(args[0])
	if err != nil {
		return err
	}
	defer clear(data)
	var invitation model.InvitationFile
	if strictjson.DecodeStrictObject(data, &invitation, client.InvitationFileBytes) != nil {
		problem := flowError("invitation_file", nil)
		problem.Context = []string{"File: " + args[0]}
		return problem
	}
	u, err := url.Parse(invitation.ServerURL)
	if err != nil {
		return flowError("input", err)
	}
	name := joinProfileName(s.app.base, u.Hostname(), s.app.env("OBERTH_PROFILE"))
	dir, err := client.ProfileDir(s.app.base, name)
	if err != nil {
		return err
	}
	if err = s.checkJoinCheckout(invitation); err != nil {
		return err
	}
	result, err := client.Join(s.ctx, s.app.config, invitation, dir)
	if err != nil {
		return err
	}
	bound, err := s.bindJoinedCheckout(dir)
	if err != nil {
		return err
	}
	if err = os.Remove(args[0]); err != nil {
		problem := flowError("invitation_remove", err)
		problem.Context = []string{"File: " + args[0]}
		return problem
	}
	return s.joined(result, name, filepath.Join(dir, client.UplinkCAFile), bound)
}

func (s *flowSession) joined(result model.InviteCertificate, name, ca string, bound bool) error {
	s.last, _ = json.Marshal(result)
	if err := s.fact("identity: %s\nuplink profile: %s", result.Identity, name); err != nil {
		return err
	}
	if current, err := os.Readlink(filepath.Join(s.app.base, "current")); err == nil {
		if err = s.fact("current profile: %s", current); err != nil {
			return err
		}
	}
	for _, scope := range result.Scope {
		if err := s.fact("scope: %s", scope); err != nil {
			return err
		}
	}
	next := "next: oberth repo list"
	if bound {
		next = "next: git push oberth HEAD:refs/heads/<new-branch> -o oberth.run=1"
	}
	if err := s.fact("%s", next); err != nil || s.options.json {
		return err
	}
	if err := s.fact("%s", joinedWebView(ca)); err != nil {
		return err
	}
	return s.app.agentTools(name)
}
func renew(s *flowSession, args []string) error {
	if !validFlowArgs("renew", args) {
		return usageProblems{"renew accepts no arguments"}
	}
	if err := s.connect(); err != nil {
		return err
	}
	key, err := client.ReadPrivate(s.resolved.ClientKey)
	if err != nil {
		return err
	}
	defer clear(key)
	csr, err := client.RenewalCSR(key)
	if err != nil {
		return err
	}
	var result apicontract.Identity
	if err = s.invoke("client_renew", apicontract.ClientRenewArgs{CSR: string(csr)}, &result); err != nil {
		return err
	}
	ca, err := client.ReadPublic(s.resolved.CAFile)
	if err != nil {
		return err
	}
	if err = client.InstallRenewed(s.resolved.ClientCert, client.Renewal{Key: key, CA: ca, Certificate: result.Certificate, Now: s.app.config.Time()}); err != nil {
		return err
	}
	return s.fact("certificate renewed\nnext: oberth system status")
}
func addRepository(s *flowSession, args []string) error {
	f := flowFlags("repo add", s.app.home())
	rest, err := parseFlow(f, args)
	known := flagValue[string](f, "known-hosts")
	if err != nil {
		return err
	}
	if !validFlowArgs("repo add", rest) {
		return usageProblems{"repo add requires a forge URL"}
	}
	u, err := parseRepositoryAddress(rest[0])
	if err != nil {
		return err
	}
	if name := *flagValue[string](f, "upstream"); name != "" {
		if err := reponame.ValidateUpstreamName(name); err != nil {
			return err
		}
	}
	connection, trusted, err := s.prepareCheckout(*known)
	if err != nil {
		return err
	}
	if connection.Uplink {
		return bindRegisteredRepository(s, u, trusted, *flagValue[string](f, "upstream"))
	}
	return registerRepository(s, rest[0], f, trusted)
}

// registerRepository takes the address as the person gave it, so a printed
// retry command repeats it exactly.
func registerRepository(s *flowSession, address string, flags *flag.FlagSet, known string) error {
	u, err := parseRepositoryAddress(address)
	if err != nil {
		return err
	}
	upstream, err := repositoryUpstream(s, u, *flagValue[string](flags, "upstream"), flags)
	if err != nil {
		return repoAddTrustFailure(err, address, known, upstream)
	}
	name, err := repositoryFromURL(upstream, u)
	if err != nil {
		return err
	}
	repo := name.Upstream + "/" + name.Organization + "/" + name.Name
	var registered apicontract.Repository
	if err = s.invoke("repo_add", apicontract.RepoAddArgs{Repo: repo, Upstream: upstream}, &registered); err != nil {
		return err
	}
	if registered.Repo == "" {
		return flowError("contract", nil)
	}
	if registered.Repo != repo {
		if err = s.checkGitBinding(registered.Repo, "repo add"); err != nil {
			return err
		}
	}
	repo = registered.Repo
	if err = s.invoke("repo_verify", apicontract.RepoVerifyArgs{Repo: repo}, &registered); err != nil {
		return err
	}
	if err = s.bindCheckout(repo, known); err != nil {
		return err
	}
	return s.repositoryBound(upstream, repo)
}

func (s *flowSession) repositoryBound(upstream, repo string) error {
	return s.fact("Upstream: %s\nStep accounts and release subjects derive from this name.\nbound: %s\nnext: git push oberth HEAD:refs/heads/<new-branch> -o oberth.run=1", upstream, repo)
}

// prepareCheckout selects the trusted host keys: an explicit file, else the
// host keys a joined profile saved from its invitation, else the user's own
// known_hosts.
func (s *flowSession) prepareCheckout(known string) (client.Profile, string, error) {
	if err := s.refuseBareCheckout(); err != nil {
		return client.Profile{}, known, err
	}
	if _, err := s.git("rev-parse", "--show-toplevel"); err != nil {
		return client.Profile{}, known, err
	}
	if err := s.connect(); err != nil {
		return client.Profile{}, known, err
	}
	if known == "" {
		if profile, _ := s.readProfile(); profile.Uplink {
			known = profile.KnownHosts
		}
	}
	if known == "" {
		known = filepath.Join(s.app.home(), ".ssh", "known_hosts")
	}
	connection, _, err := s.checkoutProfile(known)
	return connection, known, err
}

// profileHostKeys is the issuing profile's SSH host-key record, never a scan.
// Without one the invitation is refused; a record that join would not accept
// or that does not pin the installation's SSH endpoint is refused here, before
// any invitation exists.
func (s *flowSession) profileHostKeys() (string, error) {
	if s.resolved.ClientKey == "" {
		return "", nil
	}
	profile, err := s.readProfile()
	if err != nil || profile.KnownHosts == "" {
		return "", nil
	}
	data, err := client.ReadPublic(profile.KnownHosts)
	if err != nil {
		return "", nil
	}
	if err = issuedTrust(profile, string(data), s.resolved.URL); err != nil {
		problem := flowError("invite_trust", err)
		problem.Context = []string{"File: " + profile.KnownHosts}
		return "", problem
	}
	return string(data), nil
}

// issuedTrust holds a record to what join accepts and to pinning the
// installation's own SSH endpoint.
func issuedTrust(profile client.Profile, record, serverURL string) error {
	if !client.ValidHostKeys(record) {
		return errors.New("host-key record is malformed or exceeds its bound")
	}
	endpoint, err := profileSSH(profile, serverURL)
	if err != nil {
		return err
	}
	return gitssh.CheckPins(profile.KnownHosts, net.JoinHostPort(endpoint.Hostname(), sshPort(endpoint)))
}
