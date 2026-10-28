package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/releasecredential"
	"github.com/oberthci/oberth/internal/releaseimage"
	"github.com/oberthci/oberth/internal/staticgit"
)

var (
	semanticVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	commitID        = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "oberth-release-image: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string) error {
	if len(arguments) < 1 {
		return errors.New("expected publish, verify, publish-kvm, or verify-kvm")
	}
	switch arguments[0] {
	case "static-sbom":
		if len(arguments) != 3 && (len(arguments) != 4 || arguments[3] != "control") {
			return errors.New("usage: oberth-release-image static-sbom ARCH OUTPUT [control]")
		}
		output, err := os.OpenFile(arguments[2], os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600) // #nosec G304 -- explicit release metadata output path.
		if err != nil {
			return err
		}
		result := staticgit.WriteSBOM(output, arguments[1], len(arguments) == 4)
		closed := output.Close()
		if result != nil {
			return result
		}
		return closed
	case "publish", "verify":
		return runServer(ctx, arguments)
	case "publish-kvm", "verify-kvm":
		return runKVM(ctx, arguments)
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func runServer(ctx context.Context, arguments []string) error {
	if len(arguments) != 6 {
		return errors.New("usage: oberth-release-image publish|verify TAG SHA CREATED DIST GAR_ACCESS_TOKEN")
	}
	if !semanticVersion.MatchString(arguments[1]) || !commitID.MatchString(arguments[2]) {
		return errors.New("release tag or source commit is invalid")
	}
	created, err := time.Parse(time.RFC3339, arguments[3])
	if err != nil {
		return errors.New("release creation time must be RFC3339")
	}
	authenticator, err := releasecredential.FromEnvironment(ctx, arguments[5])
	if err != nil {
		return err
	}
	config := releaseimage.PublishConfig{
		Tag: arguments[1], SHA: arguments[2], Created: created, Authenticator: authenticator, Dist: arguments[4],
	}
	if arguments[0] == "verify" {
		server, err := readReference(filepath.Join(arguments[4], "server-image.txt"))
		if err != nil {
			return err
		}
		return releaseimage.Verify(ctx, config, releaseimage.Published{Server: server})
	}
	published, err := releaseimage.Publish(ctx, config)
	if err != nil {
		return err
	}
	result := struct {
		Schema int    `json:"schema"`
		SHA    string `json:"source_commit"`
		Server string `json:"server_image"`
	}{Schema: 2, SHA: arguments[2], Server: published.Server}
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	path := filepath.Join(arguments[4], "images.json")
	//nolint:gosec // path is a fixed filename beneath the explicit release-output directory.
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return err
	}
	//nolint:gosec // destination is a fixed filename beneath the explicit release-output directory.
	return os.WriteFile(filepath.Join(arguments[4], "server-image.txt"), []byte(published.Server+"\n"), 0o600)
}

func runKVM(ctx context.Context, arguments []string) error {
	if len(arguments) != 7 {
		return errors.New("usage: oberth-release-image publish-kvm|verify-kvm TAG SHA CREATED DIST GAR_ACCESS_TOKEN RUNTIME_TARBALL")
	}
	if !semanticVersion.MatchString(arguments[1]) || !commitID.MatchString(arguments[2]) {
		return errors.New("release tag or source commit is invalid")
	}
	created, err := time.Parse(time.RFC3339, arguments[3])
	if err != nil {
		return errors.New("release creation time must be RFC3339")
	}
	authenticator, err := releasecredential.FromEnvironment(ctx, arguments[5])
	if err != nil {
		return err
	}
	config := releaseimage.PublishConfig{
		Tag: arguments[1], SHA: arguments[2], Created: created, Authenticator: authenticator, Dist: arguments[4],
	}
	if arguments[0] == "verify-kvm" {
		runtime, err := readReference(filepath.Join(arguments[4], "kvm-runtime-image.txt"))
		if err != nil {
			return err
		}
		harness, err := readReference(filepath.Join(arguments[4], "kvm-harness-image.txt"))
		if err != nil {
			return err
		}
		return releaseimage.VerifyKVM(ctx, config, releaseimage.KVMPublished{Runtime: runtime, Harness: harness})
	}
	published, err := releaseimage.PublishKVM(ctx, config, arguments[6])
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "kvm-harness-digest: %s\n", published.Harness)
	return releaseimage.WriteKVMReferences(arguments[4], published)
}

func readReference(path string) (string, error) {
	//nolint:gosec // path is the explicit read-only release-reference operand.
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read release image reference: %w", err)
	}
	value := strings.TrimSuffix(string(body), "\n")
	if value+"\n" != string(body) || strings.ContainsAny(value, "\r\n\t ") {
		return "", errors.New("release image reference is not canonical")
	}
	return value, nil
}
