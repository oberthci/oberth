// Package releasecredential confines registry authority to the release WIF helper
// or the explicit legacy rollback token. It never consults ambient Docker auth.
package releasecredential

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
)

const Registry = "europe-west4-docker.pkg.dev"
const helper = "/work/src/scripts/release-wif-credential.py"

// FromEnvironment selects exactly one authentication mode. A configured WIF
// failure cannot read the legacy operand, even when that file exists.
func FromEnvironment(ctx context.Context, tokenPath string) (authn.Authenticator, error) {
	switch os.Getenv("OBERTH_RELEASE_WIF") {
	case "1":
		if tokenPath != "wif" || os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "/run/oberth-gar/adc.json" {
			return nil, errors.New("invalid release WIF configuration")
		}
		return &credential{ctx: ctx, script: helper}, nil
	case "":
		if tokenPath == "wif" {
			return nil, errors.New("WIF operand requires OBERTH_RELEASE_WIF=1")
		}
		//nolint:gosec // Explicit legacy rollback operand, never read in WIF mode.
		token, err := os.ReadFile(tokenPath)
		if err != nil || len(token) == 0 {
			return nil, errors.New("read GAR access token")
		}
		defer clear(token)
		return authn.FromConfig(authn.AuthConfig{Username: "oauth2accesstoken", Password: string(token)}), nil
	default:
		return nil, errors.New("invalid OBERTH_RELEASE_WIF value")
	}
}

type credential struct {
	ctx    context.Context
	script string
}

// Authorization exchanges on each auth request, including registry retries.
// No 600-second credential is cached across the hour-long publication leaf.
func (c *credential) Authorization() (*authn.AuthConfig, error) {
	ctx, cancel := context.WithTimeout(c.ctx, 45*time.Second)
	defer cancel()
	//nolint:gosec // Interpreter and production script are fixed, verified source paths.
	cmd := exec.CommandContext(ctx, "/usr/bin/python3", "-I", "-S", "-B", c.script, "get")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "OBERTH_RELEASE_WIF=1", "GOOGLE_APPLICATION_CREDENTIALS=/run/oberth-gar/adc.json"}
	cmd.Stdin = strings.NewReader(Registry + "\n")
	// StdoutPipe is deliberately an OS pipe: the accepted helper rejects regular
	// files and terminals. Stderr is discarded; failures never disclose payloads.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.New("keyless registry authentication failed")
	}
	if err = cmd.Start(); err != nil {
		return nil, errors.New("keyless registry authentication failed")
	}
	body, readErr := io.ReadAll(io.LimitReader(stdout, 65537))
	defer clear(body)
	if readErr != nil || len(body) > 65536 {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil || waitErr != nil || len(body) > 65536 {
		return nil, errors.New("keyless registry authentication failed")
	}
	var result struct {
		Username string
		Secret   string
	}
	if json.Unmarshal(body, &result) != nil || result.Username != "oauth2accesstoken" || result.Secret == "" || strings.ContainsAny(result.Secret, "\r\n\x00") {
		return nil, errors.New("keyless registry authentication failed")
	}
	return &authn.AuthConfig{Username: result.Username, Password: result.Secret}, nil
}
