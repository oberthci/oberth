package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/oberthci/oberth/internal/client"
)

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	output, err := command.Output()
	if len(args) > 0 && args[0] == "fetch" && strings.Contains(diagnostic.String(), gitport.ForgeCacheNotice) {
		return "", publicerr.New(publicerr.Unavailable, gitport.ForgeReason, gitport.ForgeRemedy, nil)
	}
	if len(args) > 0 && args[0] == "fetch" && strings.Contains(diagnostic.String(), gitport.ForgeUpdatingNotice) {
		return "", publicerr.New(publicerr.Unavailable, gitport.FetchUnfinishedReason, gitport.FetchUnfinishedRemedy, nil)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		exit.Stderr = diagnostic.Bytes()
	}
	if err != nil {
		return "", gitFailure(args, err)
	}
	return strings.TrimSpace(string(output)), nil
}
func (s *flowSession) git(args ...string) (string, error) {
	if s.app.git != nil {
		return s.app.git(s.ctx, args)
	}
	return runGit(s.ctx, s.app.directory, args...)
}
func (s *flowSession) repository() (string, error) {
	repo, err := s.git("config", "--get", "oberth.repository")
	if err != nil || repo == "" {
		return "", flowError("binding", err)
	}
	return repo, nil
}
func (s *flowSession) bindCheckout(repo, known string) error {
	connection, command, err := s.checkoutConnection(known)
	if err != nil {
		return err
	}
	if err = s.bindGitRemote(repo, command); err != nil {
		return err
	}
	if _, err = s.git("config", "oberth.repository", repo); err != nil {
		return err
	}
	if _, err = s.git("config", "oberth.profile", filepath.Dir(s.resolved.ClientKey)); err != nil {
		return err
	}
	if connection.Uplink {
		_, err = s.git("config", "oberth.uplinkProfile", filepath.Base(filepath.Dir(s.resolved.ClientKey)))
		return err
	}
	return s.clearGitSetting("oberth.uplinkProfile")
}

func (s *flowSession) bindGitRemote(repo, command string) error {
	address := gitBindingAddress(repo)
	for _, setting := range [][2]string{
		{"url", address},
		{"fetch", "+refs/heads/*:refs/remotes/oberth/*"},
		{"uploadpack", command + " git-upload-pack"},
		{"receivepack", command + " git-receive-pack"},
	} {
		if _, err := s.git("config", "--local", "--replace-all", "remote.oberth."+setting[0], setting[1]); err != nil {
			return err
		}
	}
	if err := s.clearGitSetting("oberth.knownHosts"); err != nil {
		return err
	}
	if err := s.clearGitSetting("remote.oberth.pushurl"); err != nil {
		return err
	}
	return s.clearGitSetting("remote.oberth.vcs")
}

func (s *flowSession) clearGitSetting(key string) error {
	_, err := s.git("config", "--local", "--unset-all", key)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 5 {
		return nil
	}
	return err
}

func (s *flowSession) checkoutConnection(known string) (client.Profile, string, error) {
	connection, endpoint, err := s.checkoutProfile(known)
	if err != nil {
		return connection, "", err
	}
	dir := filepath.Dir(s.resolved.ClientKey)
	key := connection.AdminKey
	if connection.Uplink {
		key = filepath.Join(dir, client.UplinkPushKeyFile)
	}
	command, err := s.pushCommand(dir, known, endpoint, key)
	return connection, command, err
}

// readProfile reads the selected profile's installation record.
func (s *flowSession) readProfile() (client.Profile, error) {
	var connection client.Profile
	profile, err := client.ReadPublic(filepath.Join(filepath.Dir(s.resolved.ClientKey), "installation.json"))
	if err != nil {
		return connection, err
	}
	if json.Unmarshal(profile, &connection) != nil {
		return connection, flowError("contract", nil)
	}
	return connection, nil
}

func (s *flowSession) checkoutProfile(known string) (client.Profile, *url.URL, error) {
	connection, err := s.readProfile()
	if err != nil {
		return connection, nil, err
	}
	if connection.SSHURL == "" {
		return connection, nil, flowError("contract", nil)
	}
	endpoint, err := profileSSH(connection, s.resolved.URL)
	if err != nil {
		return connection, nil, err
	}
	if err = checkCheckoutTrust(known, endpoint); err != nil {
		var problem *publicerr.Error
		if errors.As(err, &problem) && problem.ID == "ssh-trust-endpoint" {
			problem.Context = append(problem.Context, "SSH trust: "+connection.KnownHosts)
		}
		return connection, nil, err
	}
	connection.SSHURL = endpoint.String()
	return connection, endpoint, nil
}

func (s *flowSession) pushCommand(dir, known string, endpoint *url.URL, key string) (string, error) {
	absolute, err := filepath.Abs(known)
	if err != nil {
		return "", err
	}
	port := sshPort(endpoint)
	config := sshConfiguration(sshConnection{Host: endpoint.Hostname(), User: "git", Port: port, Key: key, Hosts: absolute})
	binary, err := s.app.clientExecutable()
	if err != nil {
		return "", flowError("repo_executable", err)
	}
	shim := filepath.Join(dir, "git-ssh")
	if err := client.WritePrivate(filepath.Join(dir, "ssh.config"), config); err != nil {
		return "", err
	}
	if err = s.installClientGitShim(shim, binary); err != nil {
		return "", err
	}
	return shellArgument(shim) + " --config " + shellArgument(filepath.Join(dir, "ssh.config")) + " --pack " + shellArgument("git@"+endpoint.Hostname()), nil

}

func checkHostFile(known string) error {
	hostFile, err := os.OpenFile(known, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		key := "host_read"
		if errors.Is(err, os.ErrNotExist) {
			key = "host_missing"
		} else if errors.Is(err, syscall.ELOOP) {
			key = "host_link"
		}
		return hostFileFailure(known, key, err)
	}
	info, err := hostFile.Stat()
	_ = hostFile.Close()
	if err != nil {
		return hostFileFailure(known, "host_read", err)
	}
	if !info.Mode().IsRegular() {
		return hostFileFailure(known, "host_kind", nil)
	}
	if info.Mode().Perm()&022 != 0 {
		return hostFileFailure(known, "host_mode", nil)
	}
	return nil
}
func hostFileFailure(path, key string, err error) error {
	problem := flowError(key, err)
	problem.Context = []string{"File: " + path}
	if key == "host_missing" {
		return &serverTrustError{problem}
	}
	return problem
}
func gitFailure(args []string, err error) error {
	reason, remedy := "Git checkout operation failed.", "Check this checkout and the Git operation's detail."
	if len(args) > 0 {
		switch args[0] {
		case "clone":
			reason, remedy = "Git clone failed.", "Check the repository URL, access and SSH host-key file."
		case "fetch":
			reason, remedy = "Git fetch failed.", "Check the oberth remote, connection and SSH host-key file."
		case "push":
			reason, remedy = "Git push failed.", "Check the remote refusal, access and SSH host-key file before retrying."
		case "tag":
			reason, remedy = "Git tag creation failed.", "Set the Git author identity in this checkout and check the named tag."
		}
	}
	problem := publicerr.New(publicerr.Unavailable, reason, remedy, err)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		problem.Detail = safeDisplay(strings.TrimSpace(string(exit.Stderr)))
		if strings.Contains(strings.ToLower(problem.Detail), "host key verification failed") {
			problem = publicerr.New(publicerr.Unavailable, "SSH host-key verification failed.", "Verify the server fingerprint and correct the checkout's trusted host-key file.", err)
			problem.Detail = safeDisplay(strings.TrimSpace(string(exit.Stderr)))
		}
	}
	return problem
}

func profileSSH(connection client.Profile, serverURL string) (*url.URL, error) {
	u, err := client.ValidateSSHEndpoint(connection.SSHURL, serverURL)
	if err != nil {
		return nil, flowError("input", err)
	}
	return u, nil
}

// The server's simple SSH transport obtains its port from its pinned sidecar.
func sshURLHost(u *url.URL) string {
	host := u.Hostname()
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
