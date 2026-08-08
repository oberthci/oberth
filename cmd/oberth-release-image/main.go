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

	"github.com/oberthci/oberth/internal/releaseimage"
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
	if len(arguments) != 6 || arguments[0] != "publish" && arguments[0] != "verify" {
		return errors.New("usage: oberth-release-image publish|verify TAG SHA CREATED DIST GAR_KEY_JSON")
	}
	if !semanticVersion.MatchString(arguments[1]) || !commitID.MatchString(arguments[2]) {
		return errors.New("release tag or source commit is invalid")
	}
	created, err := time.Parse(time.RFC3339, arguments[3])
	if err != nil {
		return errors.New("release creation time must be RFC3339")
	}
	//nolint:gosec // the operand is the explicit read-only projected GAR credential path.
	keyJSON, err := os.ReadFile(arguments[5])
	if err != nil {
		return errors.New("read GAR service-account key")
	}
	config := releaseimage.PublishConfig{
		Tag: arguments[1], SHA: arguments[2], Created: created, KeyJSON: keyJSON, Dist: arguments[4],
	}
	if arguments[0] == "verify" {
		server, err := readReference(filepath.Join(arguments[4], "server-image.txt"))
		if err != nil {
			return err
		}
		runner, err := readReference(filepath.Join(arguments[4], "runner-image.txt"))
		if err != nil {
			return err
		}
		return releaseimage.Verify(ctx, config, releaseimage.Published{Server: server, Runner: runner})
	}
	published, err := releaseimage.Publish(ctx, config)
	if err != nil {
		return err
	}
	result := struct {
		Schema int    `json:"schema"`
		SHA    string `json:"source_commit"`
		Server string `json:"server_image"`
		Runner string `json:"runner_image"`
	}{Schema: 1, SHA: arguments[2], Server: published.Server, Runner: published.Runner}
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
	if err := os.WriteFile(filepath.Join(arguments[4], "server-image.txt"), []byte(published.Server+"\n"), 0o600); err != nil {
		return err
	}
	//nolint:gosec // destination is a fixed filename beneath the explicit release-output directory.
	return os.WriteFile(filepath.Join(arguments[4], "runner-image.txt"), []byte(published.Runner+"\n"), 0o600)
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
