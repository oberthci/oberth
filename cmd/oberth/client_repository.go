package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/invite"
)

const clientRepositoryUsage = `Usage: oberth client repository --path <repo> --installation <public install JSON> --registered-repo <upstream/org/repo> --ssh-key <local private key> --ca-file <public CA> --client-cert <certificate> --client-key <TLS key> [flags]
Read the selected server's registered repository path and SSH host key over mTLS.
Print the exact push URL and per-install SSH binding as JSON. --consent writes
that binding and repository-local configuration. No global SSH alias is used.
Explicit connection files are required; ambient credentials are not selected.`

type repositoryClientOptions struct {
	Path, Installation, RegisteredRepo, SSHKey, CAFile, ClientCert, ClientKey, Mode string
	Assist, Consent                                                                 bool
}
type repositoryBinding struct {
	ServerID   string `json:"server_id"`
	PushURL    string `json:"push_url"`
	SSHConfig  string `json:"ssh_config"`
	SSHCommand string `json:"core_ssh_command"`
	Assist     bool   `json:"assist"`
	Configured bool   `json:"configured"`
}

func runClientRepository(ctx context.Context, args []string, output io.Writer) error {
	var o repositoryClientOptions
	flags := flag.NewFlagSet("client repository", flag.ContinueOnError)
	for _, item := range []struct {
		value      *string
		name, help string
	}{{&o.Path, "path", "local Git repository"}, {&o.Installation, "installation", "public JSON from the selected install"}, {&o.RegisteredRepo, "registered-repo", "canonical repository path registered on that server"}, {&o.SSHKey, "ssh-key", "local private SSH identity registered for this uplink"}, {&o.CAFile, "ca-file", "public server CA matching the install result's pin"}, {&o.ClientCert, "client-cert", "explicit mTLS client certificate"}, {&o.ClientKey, "client-key", "explicit local mTLS private key"}} {
		flags.StringVar(item.value, item.name, "", item.help)
	}
	flags.StringVar(&o.Mode, "mode", "remote", "remote, replace-remote or push-url")
	flags.BoolVar(&o.Assist, "assist", false, "enable repository-local assisted pushes")
	flags.BoolVar(&o.Consent, "consent", false, "apply the reviewed repository and SSH binding")
	positional, help, err := parseOperatorFlags(flags, args)
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, clientRepositoryUsage)
	}
	if len(positional) != 0 {
		return fmt.Errorf("%w: client repository takes no positional arguments", errUsage)
	}
	binding, err := configureClientRepository(ctx, o)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(binding)
}

func publicJSONFile(path string, value any) error {
	f, err := os.Open(path) //nolint:gosec // G304: bounded public metadata selected explicitly by the operator.
	if err != nil {
		return errors.New("cannot open required public metadata file")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, value) != nil {
		return errors.New("invalid or oversized public metadata JSON")
	}
	return nil
}
func shellSSHWord(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func sshConfigWord(s string) (string, error) {
	if strings.ContainsAny(s, "\x00\r\n%$~") {
		return "", errors.New("SSH binding paths must not contain control characters or SSH expansion tokens")
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
}
func repositoryGit(ctx context.Context, root string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...) //nolint:gosec // G204: fixed Git executable and separate validated arguments, never a shell.
	raw, err := command.Output()
	if err != nil {
		return "", errors.New("repository Git configuration refused; inspect local settings before retrying")
	}
	return strings.TrimSpace(string(raw)), nil
}

func configureClientRepository(ctx context.Context, o repositoryClientOptions) (repositoryBinding, error) {
	var binding repositoryBinding
	if o.Path == "" || o.Installation == "" || o.RegisteredRepo == "" || o.SSHKey == "" || o.CAFile == "" || o.ClientCert == "" || o.ClientKey == "" {
		return binding, errors.New("repository binding requires explicit install, repository, CA, certificate and local key paths")
	}
	if os.Getenv("GIT_SSH_COMMAND") != "" || os.Getenv("GIT_SSH") != "" {
		return binding, errors.New("clear GIT_SSH_COMMAND and GIT_SSH before binding a repository")
	}
	switch o.Mode {
	case "remote", "replace-remote", "push-url":
	default:
		return binding, errors.New("choose remote, replace-remote or push-url")
	}
	root, err := repositoryGit(ctx, o.Path, "rev-parse", "--show-toplevel")
	if err != nil {
		return binding, err
	}
	var installation installer.InstallResult
	if err = publicJSONFile(o.Installation, &installation); err != nil {
		return binding, err
	}
	server, err := url.Parse(installation.ServerURL)
	if err != nil || server.Scheme != "https" || server.Host == "" || server.User != nil || server.RawQuery != "" || server.Fragment != "" || server.Path != "" {
		return binding, errors.New("install result requires an explicit HTTPS server URL")
	}
	endpoint, err := url.Parse(installation.GitURL)
	if err != nil || endpoint.Scheme != "ssh" || endpoint.User == nil || endpoint.User.String() != "git" || endpoint.Hostname() != server.Hostname() || endpoint.Port() == "" || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return binding, errors.New("install result requires the matching explicit Git SSH endpoint")
	}
	portNumber, portErr := strconv.Atoi(endpoint.Port())
	if portErr != nil || portNumber < 1 || portNumber > 65535 || strconv.Itoa(portNumber) != endpoint.Port() {
		return binding, errors.New("invalid advertised SSH port")
	}
	if _, _, _, err = gitcache.ParseRepoPath(o.RegisteredRepo); err != nil || len(strings.Split(o.RegisteredRepo, "/")) != 3 {
		return binding, errors.New("select a canonical registered repository path")
	}
	o.SSHKey, err = filepath.Abs(o.SSHKey)
	if err != nil {
		return binding, err
	}
	info, err := os.Lstat(o.SSHKey)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return binding, errors.New("SSH identity must be a regular owner-only local private key file")
	}
	ca, err := os.ReadFile(o.CAFile)
	if err != nil {
		return binding, errors.New("cannot read public server CA")
	}
	block, rest := pem.Decode(bytes.TrimSpace(ca))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return binding, errors.New("provide exactly the selected server CA certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !certificate.IsCA {
		return binding, errors.New("invalid server CA certificate")
	}
	pin := sha256.Sum256(certificate.Raw)
	if installation.CAPin != hex.EncodeToString(pin[:]) {
		return binding, errors.New("server CA does not match the selected install result")
	}
	cfg := mcpFlagResult{mcpConfig: mcpConfig{URL: installation.ServerURL + "/mcp", CAFile: string(pem.EncodeToMemory(block)), ClientCert: o.ClientCert, ClientKey: o.ClientKey}}
	hostKey, fingerprint, err := readRepositoryHost(ctx, cfg, o.RegisteredRepo)
	if err != nil {
		return binding, err
	}
	sum := sha256.Sum256([]byte(installation.CAPin + "\n" + endpoint.String() + "\n" + fingerprint))
	binding.ServerID = hex.EncodeToString(sum[:])
	store := os.Getenv("CLAUDE_CONFIG_DIR")
	if store == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return binding, err
		}
		store = filepath.Join(home, ".claude")
	}
	store, err = filepath.Abs(store)
	if err != nil {
		return binding, err
	}
	directory := filepath.Join(store, "oberth", binding.ServerID)
	binding.SSHConfig = filepath.Join(directory, "ssh_config")
	binding.SSHCommand = "ssh -F " + shellSSHWord(binding.SSHConfig)
	endpoint.Path = "/" + o.RegisteredRepo + ".git"
	binding.PushURL = endpoint.String()
	binding.Assist = o.Assist
	keyWord, err := sshConfigWord(o.SSHKey)
	if err != nil {
		return binding, err
	}
	knownWord, err := sshConfigWord(filepath.Join(directory, "known_hosts"))
	if err != nil {
		return binding, err
	}
	host, err := installer.NormalizeClientHost(server.Hostname())
	if err != nil || host != server.Hostname() {
		return binding, errors.New("invalid advertised server host")
	}
	config := fmt.Sprintf("Host %s\n  HostName %s\n  Port %s\n  User git\n  IdentityFile %s\n  IdentitiesOnly yes\n  BatchMode yes\n  StrictHostKeyChecking yes\n  UserKnownHostsFile %s\n  GlobalKnownHostsFile /dev/null\n  HostKeyAlias oberth-%s\n", host, host, endpoint.Port(), keyWord, knownWord, binding.ServerID)
	known := "oberth-" + binding.ServerID + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey))) + "\n"
	// Refuse conflicting URLs/SSH commands before any write. An explicit
	// replace-remote choice authorizes replacing the repository's binding.
	current, _ := repositoryGit(ctx, root, "config", "--local", "--get", "core.sshCommand")
	if current != "" && current != binding.SSHCommand && o.Mode != "replace-remote" {
		return binding, errors.New("repository SSH command differs; review an explicit replace-remote choice")
	}
	existing, _ := repositoryGit(ctx, root, "config", "--local", "--get", "remote.oberth.url")
	pushurls, _ := repositoryGit(ctx, root, "config", "--local", "--get-all", "remote.oberth.pushurl")
	if o.Mode == "remote" && ((existing != "" && existing != binding.PushURL) || (pushurls != "" && pushurls != binding.PushURL)) {
		return binding, errors.New("repository remote differs; review an explicit replace-remote choice")
	}
	effective, err := repositoryGit(ctx, root, "ls-remote", "--get-url", binding.PushURL)
	if err != nil || effective != binding.PushURL {
		return binding, errors.New("git URL rewriting changes the selected server; remove that rewrite before configuring")
	}
	pushRewrites, _ := repositoryGit(ctx, root, "config", "--get-regexp", `^url\..*\.pushinsteadof$`)
	for _, line := range strings.Split(pushRewrites, "\n") {
		if _, prefix, ok := strings.Cut(line, " "); ok && prefix != "" && strings.HasPrefix(binding.PushURL, strings.TrimSpace(prefix)) {
			return binding, errors.New("a Git push URL rewrite matches the selected server; remove it before configuring")
		}
	}
	if o.Mode == "push-url" {
		if _, err = repositoryGit(ctx, root, "config", "--local", "--get", "remote.origin.url"); err != nil {
			return binding, errors.New("origin is missing")
		}
	}
	if !o.Consent {
		return binding, nil
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return binding, err
	}
	for path, body := range map[string]string{binding.SSHConfig: config, filepath.Join(directory, "known_hosts"): known} {
		if err = writeImmutableBinding(path, body); err != nil {
			return binding, err
		}
	}
	switch o.Mode {
	case "remote", "replace-remote":
		if _, err = repositoryGit(ctx, root, "config", "--local", "remote.oberth.url", binding.PushURL); err != nil {
			return binding, err
		}
		_, _ = repositoryGit(ctx, root, "config", "--local", "--unset-all", "remote.oberth.pushurl")
	case "push-url":
		if _, err = repositoryGit(ctx, root, "config", "--local", "--get", "remote.origin.url"); err != nil {
			return binding, err
		}
		if _, err = repositoryGit(ctx, root, "config", "--local", "--replace-all", "remote.origin.pushurl", binding.PushURL); err != nil {
			return binding, err
		}
	}
	if _, err = repositoryGit(ctx, root, "config", "--local", "core.sshCommand", binding.SSHCommand); err != nil {
		return binding, err
	}
	_, _ = repositoryGit(ctx, root, "config", "--local", "--fixed-value", "--unset-all", "push.pushOption", "oberth.assist=1")
	if o.Assist {
		if _, err = repositoryGit(ctx, root, "config", "--local", "--add", "push.pushOption", "oberth.assist=1"); err != nil {
			return binding, err
		}
	}
	for key, value := range map[string]string{"oberth.serverID": binding.ServerID, "oberth.sshConfig": binding.SSHConfig, "oberth.pushURL": binding.PushURL} {
		if _, err = repositoryGit(ctx, root, "config", "--local", key, value); err != nil {
			return binding, err
		}
	}
	binding.Configured = true
	return binding, nil
}

func writeImmutableBinding(path, body string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("existing SSH binding is not a private regular file")
		}
		existing, err := os.ReadFile(path) //nolint:gosec // G304: hash-named local binding, checked as a private regular file above.
		if err != nil || string(existing) != body {
			return errors.New("existing per-install SSH binding differs; use the original local identity or a separate Claude configuration directory")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // G304: exclusive private binding under the hash-derived selected installation directory.
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(file, body)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

func readRepositoryHost(ctx context.Context, cfg mcpFlagResult, registeredPath string) (ssh.PublicKey, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	httpClient, err := certificateMCPClient(cfg)
	if err != nil {
		return nil, "", err
	}
	defer httpClient.CloseIdleConnections()
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("repository binding refuses redirects")
	}
	call := func(name string, into any) error {
		body, _ := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: toolsCallParams{Name: name, Arguments: json.RawMessage(`{}`)}})
		response, err, _ := mcpRequestOnce(ctx, cfg, body, httpClient)
		if err != nil || response == nil || response.Error != nil {
			return errors.New("cannot read the selected server's registration over mTLS")
		}
		var result toolsCallResult
		if json.Unmarshal(response.Result, &result) != nil || result.IsError {
			return errors.New("selected server refused the registration lookup")
		}
		raw := result.StructuredContent
		if len(raw) == 0 && len(result.Content) == 1 && result.Content[0].Type == "text" {
			raw = []byte(result.Content[0].Text)
		}
		if json.Unmarshal(raw, into) != nil {
			return errors.New("invalid public registration response")
		}
		return nil
	}
	var health struct {
		Key         string `json:"ssh_host_key"`
		Fingerprint string `json:"ssh_host_key_fingerprint"`
	}
	if err = call("system_status", &health); err != nil {
		return nil, "", err
	}
	fingerprint, err := invite.SSHFingerprint([]byte(health.Key))
	if err != nil || fingerprint != health.Fingerprint {
		return nil, "", errors.New("selected server supplied no valid SSH host identity")
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(health.Key))
	if err != nil {
		return nil, "", errors.New("invalid server SSH public key")
	}
	var catalog struct {
		Repositories []struct {
			Path string `json:"git_path"`
		} `json:"repositories"`
	}
	if err = call("repo_list", &catalog); err != nil {
		return nil, "", err
	}
	found := 0
	for _, repo := range catalog.Repositories {
		if repo.Path == registeredPath {
			found++
		}
	}
	if found != 1 {
		return nil, "", errors.New("repository is not uniquely registered on the selected server")
	}
	return hostKey, fingerprint, nil
}
