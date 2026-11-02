package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/redact"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const (
	// secretExecStoreDirEnv is the variable release scripts read to find the
	// materialised secret tree. It carries the same name and meaning it had on
	// the Kubernetes Job path, so a script works unchanged under either engine.
	secretExecStoreDirEnv = "OBERTH_SECRETSTORE_DIR"

	// secretExecKVMountEnv names the KV mount holding Oberth's secrets, so the
	// translation below does not hardcode an administrator's choice. The
	// fallback matches the chart's own secretstore.kvMount default.
	secretExecKVMountEnv = "OBERTH_SECRETSTORE_KV_MOUNT"
	defaultKVMount       = "oberth"

	// secretExecFileMode and secretExecDirMode mirror the Job path's delivery:
	// the owner may read the file and nothing else, and the tree is not
	// traversable by anyone else.
	secretExecFileMode = 0o400
	secretExecDirMode  = 0o700

	// maxExecPaths bounds the number of --path flags a single invocation may
	// declare, matching the secretstore client's own limit.
	maxExecPaths = 32

	// requireSwaplessEnv is the environment variable that, when set to any
	// non-empty value, makes the swap-active check a hard failure instead of
	// a warning. The release pipeline can opt in by setting this in the Job
	// environment; the default remains warn-only to avoid breaking existing
	// setups that have not yet disabled swap.
	requireSwaplessEnv = "OBERTH_REQUIRE_SWAPLESS"

	// tmpfsMagic is the Linux filesystem magic number for tmpfs.
	// See statfs(2). Used to refuse writing secrets to non-tmpfs mounts.
	tmpfsMagic = 0x01021994
)

// Tests replace these probes to exercise failure paths without privileged
// mounts or another Unix identity. Production always uses the platform calls.
var secretExecFstatfs = defaultSecretExecFstatfs
var secretExecEUID = os.Geteuid

// swapCheckPath is the path to check for active swap devices. Tests replace
// it to avoid depending on the host's swap configuration.
var swapCheckPath = "/proc/swaps"

// swapSysBlockPath is the kernel block-device metadata root; tests use fixtures.
var swapSysBlockPath = "/sys/block"

// runSecretStoreExec is the full credential chain replacement. It logs in to
// the secret store with the Pod's own ServiceAccount identity, fetches the
// declared paths, writes them to a tmpfs-backed directory, and execs the child
// with the credential environment stripped and stdout/stderr redacted.
//
//	oberth secretstore exec --dir=/run/oberth-secrets \
//	    --path=oberth/data/release/r2-upload-token \
//	    --path=oberth/data/release/cosign-secret \
//	    -- <command> [args...]
func runSecretStoreExec(ctx context.Context, arguments []string, standardOut, errorOut io.Writer) error {
	directory, paths, command, err := parseExecArgs(arguments)
	if err != nil {
		return err
	}

	// The injected emptyDir exists before the fetch. Pin it without following
	// links, verify its owner and tmpfs backing, and tighten its mode before
	// any credential is fetched or materialised.
	root, err := prepareSecretExecRoot(directory)
	if err != nil {
		return fmt.Errorf("prepare private secret directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	// Warn (or refuse) if disk-capable or unverified swap is active -- tmpfs pages can be swapped to
	// disk, breaking the memory-only guarantee. When OBERTH_REQUIRE_SWAPLESS
	// is set, this is a hard failure; otherwise it warns to avoid breaking
	// existing setups that have not yet disabled swap.
	if checkSwapActive(swapCheckPath) {
		if os.Getenv(requireSwaplessEnv) != "" {
			return fmt.Errorf("swap is active on this node and %s is set; "+
				"refusing to write secrets — tmpfs pages may be paged to disk, "+
				"breaking the memory-only guarantee. "+
				"Disable swap (swapoff -a) or unset %s to downgrade to a warning",
				requireSwaplessEnv, requireSwaplessEnv)
		}
		_, _ = fmt.Fprintf(errorOut, "WARNING: swap is active on this node; "+
			"secrets on tmpfs may be paged to disk, breaking the memory-only guarantee. "+
			"Disable swap (swapoff -a) for full memory-only assurance.\n")
	}

	// Coordinates come from server-injected env, never from the repository.
	vaultAddr := os.Getenv("VAULT_ADDR")
	if vaultAddr == "" {
		return errors.New("VAULT_ADDR is not set; this command must run in a credentialed Oberth pipeline container")
	}
	vaultRole := os.Getenv("OBERTH_VAULT_ROLE")
	if vaultRole == "" {
		return errors.New("OBERTH_VAULT_ROLE is not set; this command must run in a credentialed Oberth pipeline container")
	}

	// Optional CA cert path from the server-injected VAULT_CACERT.
	var caPEM []byte
	if caPath := os.Getenv("VAULT_CACERT"); caPath != "" {
		body, readErr := readBoundedFile(caPath, 4<<20)
		if readErr != nil {
			return fmt.Errorf("read Vault CA certificate from VAULT_CACERT (%s): %w", caPath, readErr)
		}
		caPEM = body
	}

	client, err := secretstore.New(secretstore.Config{
		Address:   vaultAddr,
		Role:      vaultRole,
		CACertPEM: caPEM,
		// SA token at the conventional projected path.
	})
	if err != nil {
		return fmt.Errorf("configure secret store client: %w", err)
	}

	// Upstream-scoped paths arrive in their virtual form,
	// oberth/upstream/<org>/<repo>/<secret>. A CI trigger may declare nothing
	// else (internal/argojob/spec.go authorizeWithApprovalTable) and admission
	// requires every --path to match its declaration verbatim
	// (internal/argojob/spec.go), so the virtual form is the only thing that
	// can reach this point on a branch pipeline. The value itself lives at the
	// KV v2 data path, which is also what the credentialed policy grants, so
	// without this the read is issued against a path no policy covers and the
	// store answers 403. Release-tier paths are authored as <mount>/data/...
	// already and pass through untouched.
	paths = translateUpstreamPaths(paths, kvMountName())

	fetched, err := client.FetchKV(ctx, paths)
	if err != nil {
		return fmt.Errorf("fetch secrets from store: %w", err)
	}
	// FetchKV already revokes the login token internally via its deferred
	// logout call. No additional revocation needed here.

	// Validate path presence and collect secret bytes for redaction.
	// Empty values are accepted: cosign password can be an empty string for
	// unencrypted keys, matching the secretstore client (client.go) contract.
	var secrets []namedSecret
	localNames := make(map[string]struct{}, len(paths))
	for _, kvPath := range paths {
		localName := filepath.Base(kvPath)
		if _, duplicate := localNames[localName]; duplicate {
			zeroFetchedSecrets(fetched)
			return fmt.Errorf("duplicate local name %q from paths; use distinct last segments", localName)
		}
		localNames[localName] = struct{}{}

		values, ok := fetched[kvPath]
		if !ok || len(values) == 0 {
			zeroFetchedSecrets(fetched)
			return fmt.Errorf("path %q returned no keys", kvPath)
		}
		for key, value := range values {
			secrets = append(secrets, namedSecret{path: kvPath + "/" + key, value: value})
		}
	}

	// Emit a structured oberth-secret-fetch marker to stderr so the server's
	// step log captures the fetch outcome with the paths and field names but
	// without any secret values. The marker is written before the child starts,
	// so a step that crashes after a successful fetch still has the record.
	emitSecretFetchMarker(errorOut, paths, fetched)

	// Write files: <dir>/<last-path-segment>/<key>
	if err := checkSecretExecRoot(root); err != nil {
		zeroFetchedSecrets(fetched)
		return fmt.Errorf("recheck private secret directory: %w", err)
	}
	for _, kvPath := range paths {
		localName := filepath.Base(kvPath)
		values := fetched[kvPath]
		if err := writeSecretTreeFD(root, localName, values); err != nil {
			zeroFetchedSecrets(fetched)
			return err
		}
	}
	if err := checkSecretExecRoot(root); err != nil {
		zeroFetchedSecrets(fetched)
		return fmt.Errorf("recheck private secret directory before child: %w", err)
	}

	return execChild(directory, command, secrets, standardOut, errorOut)
}

// parseExecArgs extracts --dir, --path flags, and the command after --.
func parseExecArgs(arguments []string) (string, []string, []string, error) {
	separator := -1
	for i, arg := range arguments {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 && wantsHelp(arguments) {
		return "", nil, nil, commandHelpError{usage: "Usage: oberth secretstore exec --dir DIR --path PATH -- COMMAND [ARG...]"}
	}
	if separator < 0 {
		return "", nil, nil, fmt.Errorf("%w: no command given after --", errUsage)
	}
	flags := flag.NewFlagSet("secretstore exec", flag.ContinueOnError)
	directoryFlag := flags.String("dir", "", "private tmpfs root")
	var pathFlags stringList
	flags.Var(&pathFlags, "path", "approved secret path (repeatable)")
	positional, help, err := parseOperatorFlags(flags, arguments[:separator])
	if err != nil {
		return "", nil, nil, err
	}
	if help {
		return "", nil, nil, commandHelpError{usage: "Usage: oberth secretstore exec --dir DIR --path PATH -- COMMAND [ARG...]"}
	}
	if len(positional) > 0 {
		return "", nil, nil, fmt.Errorf("%w: expected flags before --", errUsage)
	}
	directory, paths, command := *directoryFlag, []string(pathFlags), arguments[separator+1:]
	if strings.TrimSpace(directory) == "" {
		return "", nil, nil, errors.New("--dir is required")
	}
	if len(paths) == 0 {
		return "", nil, nil, errors.New("at least one --path is required")
	}
	if len(paths) > maxExecPaths {
		return "", nil, nil, fmt.Errorf("too many --path flags (%d); maximum is %d", len(paths), maxExecPaths)
	}
	// Check for duplicate local names (last path segment).
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		name := filepath.Base(p)
		if _, dup := seen[name]; dup {
			return "", nil, nil, fmt.Errorf("duplicate local name %q from paths; use distinct last segments", name)
		}
		seen[name] = struct{}{}
	}
	if len(command) == 0 {
		return "", nil, nil, errors.New("no command given after --")
	}
	return directory, paths, command, nil
}

// validateSecretSegment rejects a KV field name or local path segment that
// would escape the target directory via path traversal or OS separator
// injection. Only flat, non-empty names without directory separators are
// accepted.
func validateSecretSegment(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", kind)
	}
	if value == "." || value == ".." {
		return fmt.Errorf("%s %q is a path traversal component", kind, value)
	}
	if strings.ContainsAny(value, "/\\") {
		return fmt.Errorf("%s %q contains a path separator", kind, value)
	}
	return nil
}

type namedSecret struct {
	path  string
	value []byte
}

// secretValues validates before a child can start and reports only coordinates.
func secretValues(secrets []namedSecret) ([][]byte, error) {
	values := make([][]byte, 0, len(secrets))
	for _, secret := range secrets {
		if err := redact.ValidateSecrets([][]byte{secret.value}); err != nil {
			return nil, fmt.Errorf("secret %q: %w", secret.path, err)
		}
		values = append(values, secret.value)
	}
	return values, nil
}

// execChild strips credential env, sets OBERTH_SECRETSTORE_DIR, wraps
// stdout/stderr in redaction, and execs the command.
func execChild(directory string, command []string, secrets []namedSecret, standardOut, errorOut io.Writer) error {
	values, err := secretValues(secrets)
	if err != nil {
		return err
	}
	stripped := map[string]struct{}{secretExecStoreDirEnv: {}}
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, remove := stripped[name]; remove {
			continue
		}
		if isExecStoreCredential(name) {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment, secretExecStoreDirEnv+"="+directory)

	stdoutRedactor := redact.NewWriter(standardOut, values)
	stderrRedactor := redact.NewWriter(errorOut, values)

	// #nosec G204,G702 -- the command IS the pipeline step; Oberth's admission
	// gate decides which images and commands may run, and this process only
	// forwards what that gate already accepted.
	child := exec.CommandContext(context.Background(), command[0], command[1:]...)
	child.Env = environment
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, stdoutRedactor, stderrRedactor
	// Tie child lifetime to this wrapper: if the wrapper is killed, the kernel
	// delivers SIGKILL to the child so it cannot outlive the redaction layer.
	setPdeathsig(child)

	runErr := child.Run()

	_ = stdoutRedactor.Close()
	_ = stderrRedactor.Close()

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("run %s: %w", command[0], runErr)
	}
	return nil
}

// isExecStoreCredential reports whether a variable is a store credential
// that must not leak to the child.
func isExecStoreCredential(name string) bool {
	return strings.HasPrefix(name, "VAULT_") ||
		strings.HasPrefix(name, "BAO_") ||
		strings.HasPrefix(name, "CONSUL_") ||
		name == "OBERTH_VAULT_ROLE"
}

// emitSecretFetchMarker writes one JSON marker line to stderr so the server's
// retained step log captures the fetch outcome. Only paths and field names are
// included; values are never part of the marker. The marker is best-effort:
// a write failure does not block the credential chain.
func emitSecretFetchMarker(w io.Writer, paths []string, fetched map[string]map[string][]byte) {
	keys := make(map[string][]string, len(fetched))
	for path, fields := range fetched {
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		keys[path] = names
	}
	marker := struct {
		Paths []string            `json:"paths"`
		Keys  map[string][]string `json:"keys"`
		OK    bool                `json:"ok"`
	}{Paths: paths, Keys: keys, OK: true}
	payload, err := json.Marshal(marker)
	if err != nil {
		return // best-effort
	}
	_, _ = fmt.Fprintf(w, "[oberth-secret-fetch] %s\n", payload)
}

// checkSwapActive reports swap that can persist tmpfs pages to disk. A
// missing proc file keeps the existing non-Linux behavior; an active device
// is exempt only when kernel sysfs positively identifies RAM-only zram.
func checkSwapActive(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // G304: package-owned /proc/swaps path, not user input
	if err != nil {
		return false
	}
	lines := strings.Split(string(data), "\n")
	if strings.Join(strings.Fields(lines[0]), " ") != "Filename Type Size Used Priority" {
		return true
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) != 0 && !ramOnlySwap(fields) {
			return true
		}
	}
	return false
}

// AI-INVARIANT: device names alone never establish memory-only swap. Only a
// canonical /dev/zramN partition with backing_dev exactly "none" is exempt;
// unknown, unreadable or malformed metadata follows the warning/strict gate.
func ramOnlySwap(fields []string) bool {
	if len(fields) != 5 || fields[1] != "partition" {
		return false
	}
	index, ok := strings.CutPrefix(fields[0], "/dev/zram")
	if !ok {
		return false
	}
	number, err := strconv.ParseUint(index, 10, 32)
	if err != nil || strconv.FormatUint(number, 10) != index {
		return false
	}
	for _, count := range fields[2:4] {
		if _, err := strconv.ParseUint(count, 10, 64); err != nil {
			return false
		}
	}
	if _, err := strconv.ParseInt(fields[4], 10, 32); err != nil {
		return false
	}
	backing, err := os.ReadFile(filepath.Join(swapSysBlockPath, "zram"+index, "backing_dev")) //nolint:gosec // G304: canonical numeric kernel device under the fixed sysfs root
	return err == nil && strings.TrimSpace(string(backing)) == "none"
}

// kvMountName resolves the KV mount holding Oberth's secrets.
func kvMountName() string {
	if mount := os.Getenv(secretExecKVMountEnv); mount != "" {
		return mount
	}
	return defaultKVMount
}

// translateUpstreamPaths rewrites upstream-scoped declarations to the KV data
// path their values actually live at, leaving every other path untouched. A
// path that does not parse is passed through so the store's own error surfaces
// rather than one invented here.
func translateUpstreamPaths(paths []string, kvMount string) []string {
	translated := make([]string, 0, len(paths))
	for _, path := range paths {
		scoped, upstreamScoped, err := periapsis.ParseUpstreamSecretStorePath(path)
		if err != nil || !upstreamScoped {
			translated = append(translated, path)
			continue
		}
		translated = append(translated, scoped.FetchPath(kvMount))
	}
	return translated
}
