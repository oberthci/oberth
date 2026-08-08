package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/redact"
	"github.com/oberthci/oberth/internal/runner"
	"github.com/oberthci/oberth/pkg/periapsis"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

// Store-sourced release secrets deliberately bypass the Kubernetes Secret
// snapshot: their values must never reach etcd, a Kubernetes object, or node
// disk. The controller fetches them at admission (before the Job exists),
// keeps them only in server memory, and injects them into the running release
// Pod's memory-backed volume through an exec session after the runner starts.

const maxSecretStoreSources = 32

// SecretStoreFetcher reads administrator-allowlisted KV paths from OpenBao. Only the
// Oberth server process implements it; the runner never holds secret store
// credentials.
type SecretStoreFetcher interface {
	FetchKV(ctx context.Context, paths []string) (map[string]map[string][]byte, error)
}

// ExecStreamer runs one command inside a Pod's container with the supplied
// stdin. It must return promptly when ctx is canceled and must not retain the
// stdin reader after returning.
type ExecStreamer func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, output io.Writer) error

// SecretStoreSource is the non-secret description of one store-sourced
// release secret: the delivery directory name and the KV path it came from.
// It is safe to place in Job annotations; values and value-derived digests
// never are.
type SecretStoreSource struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// SecretStoreData carries one fetched secret store secret in server memory only.
type SecretStoreData struct {
	Name   string
	Path   string
	Keys   []string
	Values map[string][]byte
}

// SecretStoreSnapshot is the admission-time, immutable in-memory snapshot of every
// declared secret store secret for one release Job.
type SecretStoreSnapshot struct {
	Secrets []SecretStoreData
}

// Empty reports whether the snapshot delivers nothing.
func (snapshot SecretStoreSnapshot) Empty() bool { return len(snapshot.Secrets) == 0 }

// Sources returns the annotation-safe metadata for the Job spec.
func (snapshot SecretStoreSnapshot) Sources() []SecretStoreSource {
	sources := make([]SecretStoreSource, 0, len(snapshot.Secrets))
	for _, secret := range snapshot.Secrets {
		sources = append(sources, SecretStoreSource{Name: secret.Name, Path: secret.Path})
	}
	return sources
}

// MaskValues returns immutable copies of every unique nonempty fetched value
// for the streaming redactor.
func (snapshot SecretStoreSnapshot) MaskValues() [][]byte {
	values := make([][]byte, 0, len(snapshot.Secrets))
	seen := make(map[string]bool)
	for _, secret := range snapshot.Secrets {
		for _, key := range secret.Keys {
			value := secret.Values[key]
			if len(value) == 0 || seen[string(value)] {
				continue
			}
			values = append(values, slices.Clone(value))
			seen[string(value)] = true
		}
	}
	return values
}

// DeliveryPayload encodes the exec-delivered wire body for the runner helper.
// The intermediate cloned values are zeroed before return so they do not
// linger in heap memory until GC.
func (snapshot SecretStoreSnapshot) DeliveryPayload() ([]byte, error) {
	payload := runner.SecretStorePayload{Version: runner.SecretStorePayloadVersion}
	for _, secret := range snapshot.Secrets {
		keys := make(map[string][]byte, len(secret.Keys))
		for _, key := range secret.Keys {
			keys[key] = slices.Clone(secret.Values[key])
		}
		payload.Secrets = append(payload.Secrets, runner.SecretStorePayloadSecret{Name: secret.Name, Keys: keys})
	}
	defer func() {
		for _, s := range payload.Secrets {
			for _, v := range s.Keys {
				ZeroBytes(v)
			}
		}
	}()
	return runner.EncodeSecretStorePayload(payload)
}

// Zero overwrites every held value. The caller invokes it after the run's
// delivery and masking lifetime ends; the snapshot must not be reused.
func (snapshot SecretStoreSnapshot) Zero() {
	for _, secret := range snapshot.Secrets {
		for _, value := range secret.Values {
			for index := range value {
				value[index] = 0
			}
		}
	}
}

// ZeroBytes overwrites one byte slice in place, for payload copies that
// outlive their snapshot.
func ZeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

// SecretStoreSnapshot validates a repository's declared secret store secrets against the
// administrator allowlist and fetches every value from OpenBao before any Job
// exists. A missing path, unreachable secret store, collision with a Kubernetes
// Secret mount, or non-allowlisted path fails release admission immediately.
func (controller *Controller) SecretStoreSnapshot(ctx context.Context, declared []periapsis.SecretStoreDeclaration, kubernetesNames []string) (SecretStoreSnapshot, error) {
	if len(declared) == 0 {
		return SecretStoreSnapshot{}, nil
	}
	if controller.secretStore == nil {
		return SecretStoreSnapshot{}, errors.New("repository declares SecretStoreSecrets but this server has no secret store configured")
	}
	if len(declared) > maxSecretStoreSources {
		return SecretStoreSnapshot{}, fmt.Errorf("repository declares %d secret store secrets, maximum is %d", len(declared), maxSecretStoreSources)
	}
	allowed := make(map[string]struct{}, len(controller.config.SecretStorePaths))
	for _, path := range controller.config.SecretStorePaths {
		allowed[path] = struct{}{}
	}
	taken := make(map[string]struct{}, len(kubernetesNames))
	for _, name := range kubernetesNames {
		taken[name] = struct{}{}
	}
	sources := make([]SecretStoreSource, 0, len(declared))
	paths := make([]string, 0, len(declared))
	seenPaths := make(map[string]struct{}, len(declared))
	for _, declaration := range declared {
		if messages := k8svalidation.IsDNS1123Subdomain(declaration.Name); len(messages) != 0 {
			return SecretStoreSnapshot{}, fmt.Errorf("secret store name %q must be a DNS-1123 subdomain: %s", declaration.Name, strings.Join(messages, ", "))
		}
		if err := periapsis.ValidateSecretStorePath(declaration.Path); err != nil {
			return SecretStoreSnapshot{}, err
		}
		if _, collision := taken[declaration.Name]; collision {
			return SecretStoreSnapshot{}, fmt.Errorf("secret store name %q collides with a declared Kubernetes release Secret", declaration.Name)
		}
		taken[declaration.Name] = struct{}{}
		if _, duplicate := seenPaths[declaration.Path]; duplicate {
			return SecretStoreSnapshot{}, fmt.Errorf("secret store path %q is declared more than once", declaration.Path)
		}
		seenPaths[declaration.Path] = struct{}{}
		if _, ok := allowed[declaration.Path]; !ok {
			return SecretStoreSnapshot{}, fmt.Errorf("repository-declared secret store path %q is not in the administrator allowlist", declaration.Path)
		}
		sources = append(sources, SecretStoreSource{Name: declaration.Name, Path: declaration.Path})
		paths = append(paths, declaration.Path)
	}
	slices.SortFunc(sources, func(left, right SecretStoreSource) int { return strings.Compare(left.Name, right.Name) })
	for index := 1; index < len(sources); index++ {
		if sources[index].Name == sources[index-1].Name {
			return SecretStoreSnapshot{}, fmt.Errorf("secret store name %q is declared more than once", sources[index].Name)
		}
	}
	fetched, err := controller.secretStore.FetchKV(ctx, paths)
	if err != nil {
		return SecretStoreSnapshot{}, fmt.Errorf("release admission requires every declared secret store secret: %w", err)
	}
	snapshot := SecretStoreSnapshot{Secrets: make([]SecretStoreData, 0, len(sources))}
	for _, source := range sources {
		values := fetched[source.Path]
		if len(values) == 0 {
			return SecretStoreSnapshot{}, fmt.Errorf("secret store entry %q is unavailable: the fetch returned no data", source.Path)
		}
		keys := make([]string, 0, len(values))
		cloned := make(map[string][]byte, len(values))
		for key, value := range values {
			if err := validateReleaseSecretKey(source.Name, key); err != nil {
				return SecretStoreSnapshot{}, fmt.Errorf("secret store entry %q: %w", source.Path, err)
			}
			if len(value) == 0 {
				return SecretStoreSnapshot{}, fmt.Errorf("secret store entry %q key %q has an empty value", source.Path, key)
			}
			keys = append(keys, key)
			cloned[key] = slices.Clone(value)
		}
		slices.Sort(keys)
		snapshot.Secrets = append(snapshot.Secrets, SecretStoreData{
			Name: source.Name, Path: source.Path, Keys: keys, Values: cloned,
		})
	}
	// The delivery wire format enforces its own bounds; proving them here keeps
	// admission failures ahead of Job creation.
	if _, err := snapshot.DeliveryPayload(); err != nil {
		return SecretStoreSnapshot{}, err
	}
	return snapshot, nil
}

type boundedOutput struct {
	limit  int
	buffer bytes.Buffer
}

func (output *boundedOutput) Write(body []byte) (int, error) {
	remaining := output.limit - output.buffer.Len()
	if remaining > 0 {
		if len(body) < remaining {
			remaining = len(body)
		}
		output.buffer.Write(body[:remaining])
	}
	return len(body), nil
}

func (output *boundedOutput) String() string {
	return strings.TrimSpace(output.buffer.String())
}

// deliverSecretStoreSecrets pushes the payload into the running release Pod with the
// runner image's own helper mode. The helper is idempotent, so every failure
// short of runner termination or context cancellation is retried; the runner
// refuses to start burns until a complete delivery verifies.
func (controller *Controller) deliverSecretStoreSecrets(ctx context.Context, runID string, payload []byte, secretValues [][]byte) error {
	if controller.execStream == nil {
		return errors.New("secret store delivery requires an exec-capable controller")
	}
	pod, err := controller.waitForPod(ctx, runID)
	if err != nil {
		return fmt.Errorf("discover release Pod for secret store delivery: %w", err)
	}
	command := []string{runnerBinaryPath, "--receive-secretstore", secretStoreMountPath}
	failures := 0
	var lastErr error
	for {
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), lastErr)
		}
		terminal, inspectErr := controller.runnerTerminated(ctx, pod.Name)
		if inspectErr == nil && terminal {
			return errors.Join(fmt.Errorf("runner terminated before secret store delivery completed"), lastErr)
		}
		output := &boundedOutput{limit: 4096}
		redacted := redact.NewWriter(output, secretValues)
		execErr := controller.execStream(ctx, controller.config.Namespace, pod.Name, runnerContainerName, command, bytes.NewReader(payload), redacted)
		_ = redacted.Close()
		if execErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), execErr)
		}
		if text := output.String(); text != "" {
			lastErr = fmt.Errorf("deliver secret store secrets to Pod %s: %w: %s", pod.Name, execErr, text)
		} else {
			lastErr = fmt.Errorf("deliver secret store secrets to Pod %s: %w", pod.Name, execErr)
		}
		failures++
		if retryErr := waitForRetry(ctx, retryDelay(failures)); retryErr != nil {
			return errors.Join(retryErr, lastErr)
		}
	}
}
