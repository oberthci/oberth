package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

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

// secretExecStatfs is the function used to check filesystem type. Tests
// replace it to avoid requiring an actual tmpfs mount.
var secretExecStatfs = defaultStatfs

// swapCheckPath is the path to check for active swap devices. Tests replace
// it to avoid depending on the host's swap configuration.
var swapCheckPath = "/proc/swaps"

func defaultStatfs(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	// Statfs_t.Type is int64 on linux (both release architectures) but uint32
	// on darwin, where the release also cross-compiles this command. The
	// explicit conversion keeps all four release targets compiling; on darwin
	// no filesystem reports TMPFS_MAGIC, so the tmpfs gate below fails closed —
	// correct, because `secretstore exec` only ever runs in Linux pipeline
	// containers.
	return int64(stat.Type), nil //nolint:unconvert // no-op on linux, required on darwin
}

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

	// Refuse to write secrets to non-tmpfs mounts. This is a runtime backstop
	// against misconfigured volume declarations that would land credentials on
	// disk.
	fsType, err := secretExecStatfs(directory)
	if err != nil {
		return fmt.Errorf("check secret directory filesystem: %w", err)
	}
	if fsType != tmpfsMagic {
		return fmt.Errorf("secret directory %s is not a tmpfs mount (type 0x%x); "+
			"refusing to write credentials to a non-memory-backed filesystem", directory, fsType)
	}

	// Warn (or refuse) if swap is active -- tmpfs pages can be swapped to
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
	// else (pkg/argoworkflow/declare.go AuthorizeSecretPaths) and admission
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
	var secrets [][]byte
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
		for _, value := range values {
			secrets = append(secrets, value)
		}
	}

	// Emit a structured oberth-secret-fetch marker to stderr so the server's
	// step log captures the fetch outcome with the paths and field names but
	// without any secret values. The marker is written before the child starts,
	// so a step that crashes after a successful fetch still has the record.
	emitSecretFetchMarker(errorOut, paths, fetched)

	// Write files: <dir>/<last-path-segment>/<key>
	// #nosec G703 -- the directory is an operator-set flag on the pipeline step,
	// not repository data; the per-secret destinations under it are validated.
	if err := os.MkdirAll(directory, secretExecDirMode); err != nil {
		zeroFetchedSecrets(fetched)
		return fmt.Errorf("create secret directory: %w", err)
	}
	for _, kvPath := range paths {
		localName := filepath.Base(kvPath)
		values := fetched[kvPath]
		if err := writeSecretTree(directory, localName, values); err != nil {
			zeroFetchedSecrets(fetched)
			return err
		}
	}

	return execChild(directory, command, secrets, standardOut, errorOut)
}

// parseExecArgs extracts --dir, --path flags, and the command after --.
func parseExecArgs(arguments []string) (string, []string, []string, error) {
	directory := ""
	var paths []string
	index := 0

	for ; index < len(arguments); index++ {
		arg := arguments[index]
		if arg == "--" {
			index++
			break
		}
		// --dir=value or --dir value
		if arg == "--dir" || arg == "-dir" {
			if index+1 >= len(arguments) {
				return "", nil, nil, errors.New("--dir requires a value")
			}
			index++
			directory = arguments[index]
			continue
		}
		if rest, found := strings.CutPrefix(arg, "--dir="); found {
			directory = rest
			continue
		}
		if rest, found := strings.CutPrefix(arg, "-dir="); found {
			directory = rest
			continue
		}
		// --path=value or --path value
		if arg == "--path" || arg == "-path" {
			if index+1 >= len(arguments) {
				return "", nil, nil, errors.New("--path requires a value")
			}
			index++
			paths = append(paths, arguments[index])
			continue
		}
		if rest, found := strings.CutPrefix(arg, "--path="); found {
			paths = append(paths, rest)
			continue
		}
		if rest, found := strings.CutPrefix(arg, "-path="); found {
			paths = append(paths, rest)
			continue
		}
		return "", nil, nil, fmt.Errorf("unknown flag %q; expected --dir, --path, or --", arg)
	}

	command := arguments[index:]
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

// writeSecretTree writes one secret's keys under <dir>/<name>/<key>.
func writeSecretTree(directory, name string, values map[string][]byte) error {
	if err := validateSecretSegment("local name", name); err != nil {
		return err
	}
	parent := filepath.Join(directory, name)
	// #nosec G703 -- name is the last segment of a validated secret-store path.
	if err := os.MkdirAll(parent, secretExecDirMode); err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	for key, value := range values {
		if err := validateSecretSegment("KV field name", key); err != nil {
			return err
		}
		target := filepath.Join(parent, key)
		// O_EXCL: a second write to the same path means two paths disagree about
		// what belongs there.
		// #nosec G304,G703 -- target is confined to the operator-set directory.
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, secretExecFileMode)
		if err != nil {
			return fmt.Errorf("create %s/%s: %w", name, key, err)
		}
		if _, err := file.Write(value); err != nil {
			_ = file.Close()
			return fmt.Errorf("write %s/%s: %w", name, key, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close %s/%s: %w", name, key, err)
		}
	}
	return nil
}

// execChild strips credential env, sets OBERTH_SECRETSTORE_DIR, wraps
// stdout/stderr in redaction, and execs the command.
func execChild(directory string, command []string, secrets [][]byte, standardOut, errorOut io.Writer) error {
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

	stdoutRedactor := redact.NewWriter(standardOut, secrets)
	stderrRedactor := redact.NewWriter(errorOut, secrets)

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

// --- materialize subcommand ---

// materializeMapping represents one VAR=name/key mapping.
type materializeMapping struct {
	variable string
	relative string
}

// runSecretStoreMaterialize is the transitional envconsul-compatible mode.
// It reads secrets from environment variables (set by envconsul), writes them
// to files, strips credential env vars, and execs the child.
//
//	oberth secretstore materialize --dir=/run/oberth-secrets \
//	    VAR=name/key [...] -- <command> [args...]
func runSecretStoreMaterialize(_ context.Context, arguments []string, standardOut, errorOut io.Writer) error {
	directory, mappings, command, err := parseMaterializeArgs(arguments)
	if err != nil {
		return err
	}

	// #nosec G703 -- the directory is an operator-set flag on the pipeline step.
	if err := os.MkdirAll(directory, secretExecDirMode); err != nil {
		return fmt.Errorf("create secret directory: %w", err)
	}

	secrets := make([][]byte, 0, len(mappings))
	for _, entry := range mappings {
		value, present := os.LookupEnv(entry.variable)
		if !present {
			return fmt.Errorf("%s is not set: envconsul resolved no such field "+
				"(check the KV field names at the declared secret paths)", entry.variable)
		}
		// Empty values are accepted: cosign password can be an empty string
		// for unencrypted keys, matching the secretstore client contract.
		if err := writeMaterializeSecret(directory, entry.relative, value); err != nil {
			return err
		}
		secrets = append(secrets, []byte(value))
	}

	return executeMaterialize(directory, mappings, command, secrets, standardOut, errorOut)
}

// parseMaterializeArgs splits the materialize arguments.
func parseMaterializeArgs(arguments []string) (string, []materializeMapping, []string, error) {
	directory := ""
	var mappings []materializeMapping
	index := 0
	for ; index < len(arguments); index++ {
		arg := arguments[index]
		if arg == "--" {
			index++
			break
		}
		if arg == "-dir" || arg == "--dir" {
			if index+1 >= len(arguments) {
				return "", nil, nil, errors.New("-dir needs a value")
			}
			index++
			directory = arguments[index]
			continue
		}
		if rest, found := strings.CutPrefix(arg, "-dir="); found {
			directory = rest
			continue
		}
		if rest, found := strings.CutPrefix(arg, "--dir="); found {
			directory = rest
			continue
		}
		variable, relative, found := strings.Cut(arg, "=")
		if !found {
			return "", nil, nil, fmt.Errorf("expected VARIABLE=path/to/file, got %q", arg)
		}
		if strings.TrimSpace(variable) == "" {
			return "", nil, nil, fmt.Errorf("mapping %q names no variable", arg)
		}
		mappings = append(mappings, materializeMapping{variable: variable, relative: relative})
	}
	command := arguments[index:]
	if strings.TrimSpace(directory) == "" {
		return "", nil, nil, errors.New("-dir is required")
	}
	if len(mappings) == 0 {
		return "", nil, nil, errors.New("no VARIABLE=path mappings given")
	}
	if len(command) == 0 {
		return "", nil, nil, errors.New("no command given after --")
	}
	return directory, mappings, command, nil
}

// writeMaterializeSecret places one value, refusing any relative path that
// would leave the directory.
func writeMaterializeSecret(directory, relative, value string) error {
	if err := validateMaterializeRelative(relative); err != nil {
		return err
	}
	name, key, _ := strings.Cut(relative, "/")
	parent := filepath.Join(directory, name)
	// #nosec G703 -- validateMaterializeRelative rejects absolute paths,
	// traversal, and anything that is not exactly <name>/<key>.
	if err := os.MkdirAll(parent, secretExecDirMode); err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	target := filepath.Join(parent, key)
	// #nosec G304,G703 -- same validation; target is confined to the directory.
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, secretExecFileMode)
	if err != nil {
		return fmt.Errorf("create %s: %w", relative, err)
	}
	if _, err := file.WriteString(value); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", relative, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", relative, err)
	}
	return nil
}

func validateMaterializeRelative(relative string) error {
	cleaned := filepath.Clean(relative)
	if relative == "" {
		return errors.New("mapping has an empty destination path")
	}
	if filepath.IsAbs(relative) || strings.HasPrefix(cleaned, "..") {
		return fmt.Errorf("destination %q must be relative to the secret directory", relative)
	}
	if cleaned != relative {
		return fmt.Errorf("destination %q must be a clean relative path", relative)
	}
	segments := strings.Split(cleaned, string(os.PathSeparator))
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return fmt.Errorf("destination %q must be <name>/<key>", relative)
	}
	return nil
}

// executeMaterialize runs the real command with mapped variables stripped.
func executeMaterialize(directory string, mappings []materializeMapping, command []string, secrets [][]byte, standardOut, errorOut io.Writer) error {
	stripped := map[string]struct{}{secretExecStoreDirEnv: {}}
	for _, entry := range mappings {
		stripped[entry.variable] = struct{}{}
	}
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, remove := stripped[name]; remove {
			continue
		}
		if isMaterializeStoreCredential(name) {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment, secretExecStoreDirEnv+"="+directory)

	stdoutRedactor := redact.NewWriter(standardOut, secrets)
	stderrRedactor := redact.NewWriter(errorOut, secrets)

	// #nosec G204,G702 -- same justification as the original shim.
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

func isMaterializeStoreCredential(name string) bool {
	return strings.HasPrefix(name, "VAULT_") ||
		strings.HasPrefix(name, "BAO_") ||
		strings.HasPrefix(name, "CONSUL_")
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

// checkSwapActive reads the swap status file and returns true if any swap
// devices are active. Returns false on read error (the file may not exist
// on non-Linux systems). Swapless nodes are a prerequisite for the memory-only
// secret delivery contract: tmpfs pages can be paged to disk when swap is
// active, which breaks the guarantee that secrets never touch persistent
// storage.
func checkSwapActive(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is a package-level default (/proc/swaps), not user input
	if err != nil {
		return false
	}
	// /proc/swaps has a header line; any subsequent non-empty line means
	// a swap device is active.
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) != "" {
			return true
		}
	}
	return false
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
