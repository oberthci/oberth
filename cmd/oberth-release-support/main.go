package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"golang.org/x/mod/semver"
	"sigs.k8s.io/yaml"
)

const (
	defaultCloudflareAPIBase = "https://api.cloudflare.com/client/v4"
	maximumResponseBytes     = 1 << 20
	maximumIndexBytes        = 8 << 20
	garHost                  = "europe-west4-docker.pkg.dev"
	oberthChartRepository    = "skipopsmain/cloudtaser-helm/oberth"
	releaseHeartbeatInterval = 15 * time.Second
	releaseCommandWaitDelay  = 500 * time.Millisecond
	repositoryReleaseScript  = "./.oberth/release.sh"
	maximumReleaseLineBytes  = 64 << 10
	runnerOwnsGroupEnv       = "OBERTH_RUNNER_OWNS_RELEASE_GROUP"
)

var (
	accountPattern    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	bucketPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefixPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*/$`)
	tokenPattern      = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	credentialPattern = regexp.MustCompile(`^[A-Za-z0-9/+=._-]+$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// Helm index.yaml uses bare hexadecimal chart digests, unlike OCI references.
	chartDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	heartbeatPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

type credentials struct {
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "oberth-release-support: %v\n", err)
		os.Exit(releaseCommandExitCode(err))
	}
}

func run(ctx context.Context, arguments []string, output io.Writer) error {
	switch {
	case len(arguments) == 3 && arguments[0] == "semver-compare":
		if !semver.IsValid(arguments[1]) || !semver.IsValid(arguments[2]) {
			return errors.New("semver-compare requires two valid v-prefixed semantic versions")
		}
		_, err := fmt.Fprintln(output, semver.Compare(arguments[1], arguments[2]))
		return err
	case len(arguments) == 3 && arguments[0] == "normalize-tree":
		created, err := time.Parse(time.RFC3339, arguments[2])
		if err != nil {
			return errors.New("normalize-tree requires an RFC3339 creation time")
		}
		return normalizeTree(arguments[1], created)
	case len(arguments) == 5 && arguments[0] == "chart-index-state":
		// #nosec G602 -- this switch arm proves the exact five-argument shape.
		state, err := inspectChartIndex(arguments[1], arguments[2], arguments[3], arguments[4])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, state)
		return err
	case len(arguments) == 3 && arguments[0] == "registry-digest":
		digest, err := resolveChartDigest(ctx, arguments[1], arguments[2])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, digest)
		return err
	case len(arguments) == 6 && arguments[0] == "r2-auth":
		return runR2Auth(ctx, arguments)
	case len(arguments) >= 4 && arguments[0] == "heartbeat":
		return runReleaseHeartbeat(ctx, arguments[1], arguments[2], arguments[3:], output, releaseHeartbeatInterval)
	default:
		return errors.New("usage: oberth-release-support semver-compare LEFT RIGHT | normalize-tree DIR CREATED | chart-index-state INDEX VERSION DIGEST URL | registry-digest REF KEY_JSON | r2-auth TOKEN_FILE ACCOUNT_ID BUCKET PREFIX OUTPUT | heartbeat LABEL COMMAND [ARG...]")
	}
}

type synchronizedWriter struct {
	mu      sync.Mutex
	output  io.Writer
	pending []byte
	err     error
	failed  chan<- error
}

func (writer *synchronizedWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.err != nil {
		return 0, writer.err
	}

	originalLength := len(value)
	consumed := 0
	for len(value) > 0 {
		// Bare carriage returns stay inside the child record. The deployed
		// predecessor derives secret fragments only at LF/CRLF boundaries, so
		// translating a bare CR could split a secret before that masker sees it.
		delimiter := bytes.IndexByte(value, '\n')
		if delimiter < 0 {
			if err := writer.appendPartialLocked(value); err != nil {
				return consumed, err
			}
			return originalLength, nil
		}
		if err := writer.appendPartialLocked(value[:delimiter]); err != nil {
			return consumed, err
		}
		consumed += delimiter + 1
		value = value[delimiter+1:]
		if err := writer.emitPendingLineLocked(); err != nil {
			return consumed, err
		}
	}
	return originalLength, nil
}

func (writer *synchronizedWriter) heartbeat(label string) (bool, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.err != nil {
		return false, writer.err
	}
	// Child fragments remain private in pending until their record boundary, so
	// a heartbeat can never split a secret before the downstream masker sees it.
	value := []byte(fmt.Sprintf("release heartbeat: %s active\n", label))
	if err := writer.writeLocked(value); err != nil {
		return false, err
	}
	return true, nil
}

func (writer *synchronizedWriter) Flush() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.err != nil {
		return writer.err
	}
	if len(writer.pending) == 0 {
		return nil
	}
	if err := writer.writeLocked(writer.pending); err != nil {
		return err
	}
	writer.pending = nil
	return nil
}

func (writer *synchronizedWriter) appendPartialLocked(value []byte) error {
	if len(writer.pending)+len(value) > maximumReleaseLineBytes {
		return writer.failLocked(fmt.Errorf("release command output line exceeds %d bytes", maximumReleaseLineBytes))
	}
	writer.pending = append(writer.pending, value...)
	return nil
}

func (writer *synchronizedWriter) emitPendingLineLocked() error {
	line := make([]byte, len(writer.pending)+1)
	copy(line, writer.pending)
	line[len(line)-1] = '\n'
	if err := writer.writeLocked(line); err != nil {
		return err
	}
	writer.pending = writer.pending[:0]
	return nil
}

func (writer *synchronizedWriter) writeLocked(value []byte) error {
	written, err := writer.output.Write(value)
	if written != len(value) && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil {
		return writer.failLocked(err)
	}
	return nil
}

func (writer *synchronizedWriter) failLocked(err error) error {
	if writer.err == nil {
		writer.err = err
		if writer.failed != nil {
			select {
			case writer.failed <- err:
			default:
			}
		}
	}
	return writer.err
}

func runReleaseHeartbeat(
	ctx context.Context,
	label string,
	commandName string,
	arguments []string,
	output io.Writer,
	interval time.Duration,
) error {
	if !heartbeatPattern.MatchString(label) {
		return errors.New("heartbeat label must contain only lowercase letters, digits, and hyphens")
	}
	if commandName != repositoryReleaseScript || len(arguments) != 1 {
		return errors.New("heartbeat accepts one repository release action")
	}
	if interval <= 0 {
		return errors.New("heartbeat interval must be positive")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if output == nil {
		output = io.Discard
	}
	command, err := releaseActionCommand(ctx, arguments[0])
	if err != nil {
		return err
	}
	return runHeartbeatCommand(label, command, output, interval)
}

func releaseActionCommand(ctx context.Context, action string) (*exec.Cmd, error) {
	switch action {
	case "build":
		return exec.CommandContext(ctx, repositoryReleaseScript, "build"), nil
	case "publish-r2":
		return exec.CommandContext(ctx, repositoryReleaseScript, "publish-r2"), nil
	case "publish-images":
		return exec.CommandContext(ctx, repositoryReleaseScript, "publish-images"), nil
	case "package-chart":
		return exec.CommandContext(ctx, repositoryReleaseScript, "package-chart"), nil
	case "publish-chart":
		return exec.CommandContext(ctx, repositoryReleaseScript, "publish-chart"), nil
	case "verify":
		return exec.CommandContext(ctx, repositoryReleaseScript, "verify"), nil
	case "finalize":
		return exec.CommandContext(ctx, repositoryReleaseScript, "finalize"), nil
	default:
		return nil, errors.New("heartbeat release action is not allowed")
	}
}

func runHeartbeatCommand(label string, command *exec.Cmd, output io.Writer, interval time.Duration) error {
	outputErrors := make(chan error, 1)
	lockedOutput := &synchronizedWriter{output: output, failed: outputErrors}
	runnerOwnsGroup, err := configureHeartbeatCommand(command)
	if err != nil {
		return err
	}
	command.Stdout = lockedOutput
	command.Stderr = lockedOutput
	command.WaitDelay = releaseCommandWaitDelay
	if err := command.Start(); err != nil {
		return fmt.Errorf("start heartbeat command %s: %w", label, err)
	}
	if _, err := lockedOutput.heartbeat(label); err != nil {
		terminationErr := terminateHeartbeatCommand(command, runnerOwnsGroup)
		return errors.Join(fmt.Errorf("write initial release heartbeat: %w", err), terminationErr, command.Wait())
	}

	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case waitErr := <-finished:
			lingering, inspectErr := releaseGroupHasLiveMembers(command, runnerOwnsGroup)
			if lingering {
				inspectErr = errors.Join(inspectErr, errors.New("release command left a background process"))
			}
			if inspectErr != nil {
				waitErr = errors.Join(waitErr, inspectErr, terminateHeartbeatCommand(command, runnerOwnsGroup))
			}
			if waitErr != nil {
				waitErr = fmt.Errorf("heartbeat command %s: %w", label, waitErr)
			}
			flushErr := lockedOutput.Flush()
			if flushErr != nil {
				flushErr = fmt.Errorf("flush release output: %w", flushErr)
			}
			return errors.Join(waitErr, flushErr)
		case outputErr := <-outputErrors:
			terminationErr := terminateHeartbeatCommand(command, runnerOwnsGroup)
			waitErr := <-finished
			return errors.Join(fmt.Errorf("write release output: %w", outputErr), terminationErr, waitErr)
		case <-ticker.C:
			if _, err := lockedOutput.heartbeat(label); err != nil {
				terminationErr := terminateHeartbeatCommand(command, runnerOwnsGroup)
				waitErr := <-finished
				return errors.Join(fmt.Errorf("write periodic release heartbeat: %w", err), terminationErr, waitErr)
			}
		}
	}
}

// configureHeartbeatCommand leaves the release child in the helper's process
// group when the outer runner made this helper the group leader. For direct
// local invocation only, it isolates the child and makes context cancellation
// kill that complete group instead of only its shell leader.
func configureHeartbeatCommand(command *exec.Cmd) (bool, error) {
	runnerOwnsGroup := os.Getenv(runnerOwnsGroupEnv) == "1"
	if runnerOwnsGroup {
		if os.Getpid() != syscall.Getpgrp() {
			return false, errors.New("runner-owned release helper is not its process-group leader")
		}
		return true, nil
	}
	attributes := syscall.SysProcAttr{}
	if command.SysProcAttr != nil {
		attributes = *command.SysProcAttr
	}
	attributes.Setpgid = true
	command.SysProcAttr = &attributes
	if command.Cancel != nil {
		command.Cancel = func() error {
			if command.Process == nil {
				return os.ErrProcessDone
			}
			if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					return os.ErrProcessDone
				}
				return err
			}
			return nil
		}
	}
	return false, nil
}

// terminateHeartbeatCommand atomically kills the complete release process
// group. In the runner-owned case this necessarily kills the helper too, which
// is the fail-closed outcome once its output channel cannot report errors.
func terminateHeartbeatCommand(command *exec.Cmd, runnerOwnsGroup bool) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	group := command.Process.Pid
	context := "locally isolated"
	if runnerOwnsGroup {
		group = syscall.Getpgrp()
		context = "runner-owned"
	}
	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("kill %s release process group: %w", context, err)
	}
	if runnerOwnsGroup {
		return errors.New("runner-owned release process-group kill returned")
	}
	return nil
}

func releaseGroupHasLiveMembers(command *exec.Cmd, runnerOwnsGroup bool) (bool, error) {
	if command == nil || command.Process == nil {
		return false, os.ErrProcessDone
	}
	group := command.Process.Pid
	excludedPID := 0
	if runnerOwnsGroup {
		group = syscall.Getpgrp()
		excludedPID = os.Getpid()
	}
	root, err := os.OpenRoot("/proc")
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == excludedPID {
			continue
		}
		processGroup, err := syscall.Getpgid(pid)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			return false, fmt.Errorf("inspect process %d group: %w", pid, err)
		}
		if processGroup != group {
			continue
		}
		body, err := root.ReadFile(entry.Name() + "/stat")
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return false, fmt.Errorf("inspect release process %d state: %w", pid, err)
		}
		closeParen := bytes.LastIndexByte(body, ')')
		if closeParen < 0 {
			return false, fmt.Errorf("inspect release process %d state: malformed stat", pid)
		}
		fields := bytes.Fields(body[closeParen+1:])
		if len(fields) == 0 {
			return false, fmt.Errorf("inspect release process %d state: missing state", pid)
		}
		if string(fields[0]) != "Z" {
			return true, nil
		}
	}
	return false, nil
}

func releaseCommandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code > 0 && code <= 255 {
			return code
		}
	}
	return 1
}

func runR2Auth(ctx context.Context, arguments []string) error {
	if len(arguments) != 6 || arguments[0] != "r2-auth" {
		return errors.New("r2-auth requires TOKEN_FILE ACCOUNT_ID BUCKET PREFIX OUTPUT")
	}
	// #nosec G703 -- the release contract supplies a read-only projected Secret path.
	tokenBody, err := os.ReadFile(arguments[1])
	if err != nil {
		return errors.New("read R2 parent API token")
	}
	apiBase := os.Getenv("OBERTH_CLOUDFLARE_API_BASE")
	if apiBase == "" {
		apiBase = defaultCloudflareAPIBase
	}
	parsed, err := url.Parse(apiBase)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("cloudflare API base must be an HTTPS origin")
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	issued, err := exchange(ctx, client, apiBase, strings.TrimSpace(string(tokenBody)), arguments[2], arguments[3], arguments[4])
	if err != nil {
		return err
	}
	return writeCurlConfig(arguments[5], issued)
}

func normalizeTree(root string, created time.Time) error {
	// #nosec G703 -- root is the explicit chart-source operand and is rejected when it is a symlink or non-directory.
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect chart source: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("chart source must be a real directory")
	}
	created = created.UTC().Truncate(time.Second)
	var paths []string
	// #nosec G703 -- WalkDir is deliberately scoped to the validated chart-source operand.
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() && !entryInfo.IsDir() {
			return fmt.Errorf("chart source contains unsupported entry %s", path)
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return err
	}
	for index := len(paths) - 1; index >= 0; index-- {
		if err := os.Chtimes(paths[index], created, created); err != nil {
			return fmt.Errorf("normalize %s: %w", paths[index], err)
		}
	}
	return nil
}

type chartIndex struct {
	APIVersion string                       `json:"apiVersion"`
	Entries    map[string][]chartIndexEntry `json:"entries"`
}

type chartIndexEntry struct {
	Version string   `json:"version"`
	Digest  string   `json:"digest"`
	URLs    []string `json:"urls"`
}

func inspectChartIndex(indexPath, version, digest, chartURL string) (string, error) {
	if !semver.IsValid("v"+version) || !chartDigestPattern.MatchString(digest) {
		return "", errors.New("chart index expectation has an invalid version or digest")
	}
	parsedURL, err := url.Parse(chartURL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return "", errors.New("chart index URL must be an HTTPS object URL")
	}
	//nolint:gosec // indexPath is the explicit read-only chart-index operand.
	file, err := os.Open(indexPath)
	if err != nil {
		return "", fmt.Errorf("open chart index: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maximumIndexBytes+1))
	if err != nil {
		return "", fmt.Errorf("read chart index: %w", err)
	}
	if len(body) > maximumIndexBytes {
		return "", errors.New("chart index exceeds the size limit")
	}
	var index chartIndex
	if err := yaml.Unmarshal(body, &index); err != nil {
		return "", fmt.Errorf("decode chart index: %w", err)
	}
	if index.APIVersion != "v1" {
		return "", errors.New("chart index apiVersion is not v1")
	}
	matched := 0
	exact := false
	for _, entry := range index.Entries["oberth"] {
		if entry.Version != version {
			continue
		}
		matched++
		exact = entry.Digest == digest && len(entry.URLs) == 1 && entry.URLs[0] == chartURL
	}
	if matched == 0 {
		return "absent", nil
	}
	if matched != 1 || !exact {
		return "", fmt.Errorf("chart index already contains conflicting oberth version %s", version)
	}
	return "exact", nil
}

func resolveChartDigest(ctx context.Context, reference, keyPath string) (string, error) {
	parsed, err := name.ParseReference(reference, name.StrictValidation)
	if err != nil || parsed.Context().RegistryStr() != garHost || parsed.Context().RepositoryStr() != oberthChartRepository {
		return "", errors.New("registry-digest accepts only the fixed Oberth chart repository")
	}
	//nolint:gosec // keyPath is the explicit projected GAR credential operand.
	keyJSON, err := os.ReadFile(keyPath)
	if err != nil || len(keyJSON) == 0 {
		return "", errors.New("read GAR service-account key")
	}
	requestContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	descriptor, err := remote.Head(parsed, remote.WithContext(requestContext), remote.WithAuth(authn.FromConfig(authn.AuthConfig{
		Username: "_json_key",
		Password: string(keyJSON),
	})), remote.WithUserAgent("oberth-release"))
	if err != nil {
		var transportError *transport.Error
		if errors.As(err, &transportError) && transportError.StatusCode == http.StatusNotFound {
			return "missing", nil
		}
		return "", fmt.Errorf("resolve immutable GAR chart digest: %w", err)
	}
	digest := descriptor.Digest.String()
	if !digestPattern.MatchString(digest) {
		return "", errors.New("GAR returned an invalid chart digest")
	}
	return digest, nil
}

func exchange(ctx context.Context, client *http.Client, apiBase, token, account, bucket, prefix string) (credentials, error) {
	if client == nil || !tokenPattern.MatchString(token) || !accountPattern.MatchString(account) || !bucketPattern.MatchString(bucket) ||
		!prefixPattern.MatchString(prefix) || strings.Contains(prefix, "..") || strings.HasPrefix(prefix, "/") {
		return credentials{}, errors.New("R2 temporary-credential request is invalid")
	}
	var verification struct {
		Success bool `json:"success"`
		Result  struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := requestJSON(ctx, client, http.MethodGet, strings.TrimRight(apiBase, "/")+"/user/tokens/verify", token, nil, &verification); err != nil {
		return credentials{}, fmt.Errorf("verify R2 parent API token: %w", err)
	}
	if !verification.Success || verification.Result.Status != "active" || !accountPattern.MatchString(verification.Result.ID) {
		return credentials{}, errors.New("R2 parent API token verification failed")
	}
	requestBody := struct {
		Bucket            string   `json:"bucket"`
		ParentAccessKeyID string   `json:"parentAccessKeyId"`
		Permission        string   `json:"permission"`
		TTLSeconds        int      `json:"ttlSeconds"`
		Prefixes          []string `json:"prefixes"`
	}{bucket, verification.Result.ID, "object-read-write", 1800, []string{prefix}}
	var response struct {
		Success bool `json:"success"`
		Result  struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
		} `json:"result"`
	}
	endpoint := fmt.Sprintf("%s/accounts/%s/r2/temp-access-credentials", strings.TrimRight(apiBase, "/"), account)
	if err := requestJSON(ctx, client, http.MethodPost, endpoint, token, requestBody, &response); err != nil {
		return credentials{}, fmt.Errorf("request temporary R2 credentials: %w", err)
	}
	issued := credentials{response.Result.AccessKeyID, response.Result.SecretAccessKey, response.Result.SessionToken}
	if !response.Success || !validCredentials(issued) {
		return credentials{}, errors.New("temporary R2 credential response is invalid")
	}
	return issued, nil
}

func requestJSON(ctx context.Context, client *http.Client, method, endpoint, token string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	// #nosec G704 -- callers construct endpoints beneath the validated fixed HTTPS Cloudflare origin.
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// #nosec G704 -- redirects are disabled and requestJSON receives only the validated Cloudflare origin.
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumResponseBytes))
		return fmt.Errorf("cloudflare API returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maximumResponseBytes)).Decode(output); err != nil {
		return fmt.Errorf("decode Cloudflare API response: %w", err)
	}
	return nil
}

func validCredentials(value credentials) bool {
	return credentialPattern.MatchString(value.accessKeyID) && credentialPattern.MatchString(value.secretAccessKey) && credentialPattern.MatchString(value.sessionToken)
}

func writeCurlConfig(path string, value credentials) error {
	if !validCredentials(value) {
		return errors.New("temporary R2 credentials are invalid")
	}
	info, err := os.Lstat(path) // #nosec G703 -- path is the explicit private curl-config output operand.
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("curl config path must not be a symlink")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".r2-curl-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		//nolint:gosec // temporaryPath is returned by os.CreateTemp in the selected destination directory.
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	_, writeErr := fmt.Fprintf(temporary,
		"aws-sigv4 = \"aws:amz:auto:s3\"\nuser = \"%s:%s\"\nheader = \"x-amz-security-token: %s\"\n",
		value.accessKeyID, value.secretAccessKey, value.sessionToken,
	)
	closeErr := temporary.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	return os.Rename(temporaryPath, path) // #nosec G703 -- destination is the validated non-symlink output operand.
}
