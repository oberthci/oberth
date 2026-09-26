package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// SecretStoreVerifier is wired from the running server's operator-owned configuration.
type SecretStoreVerifier func(context.Context, api.SecretStoreVerifyRequest) (api.SecretStoreVerifyResponse, error)

func (service *API) verifySecretStore(ctx context.Context, actor api.Actor, raw json.RawMessage) (api.SecretStoreVerifyResponse, error) {
	// AI-CONTRACT: reject non-admin uplinks before reading configuration,
	// requesting a ServiceAccount token, or contacting the secret store.
	if !actor.Admin {
		return api.SecretStoreVerifyResponse{}, fmt.Errorf("%w: secretstore_verify requires an admin uplink", ErrForbidden)
	}
	var arguments api.SecretStoreVerifyRequest
	if err := decodeTool(raw, &arguments); err != nil {
		return api.SecretStoreVerifyResponse{}, err
	}
	if err := validateSecretStoreVerifyArguments(&arguments); err != nil {
		return api.SecretStoreVerifyResponse{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if service.secretStoreVerifier == nil {
		return api.SecretStoreVerifyResponse{}, fmt.Errorf("%w: secret-store verification is not configured", ErrUnavailable)
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, time.Duration(arguments.Timeout)*time.Second)
	defer cancel()
	if err := deadlineCtx.Err(); err != nil {
		return api.SecretStoreVerifyResponse{}, err
	}
	return service.secretStoreVerifier(deadlineCtx, arguments)
}

func validateSecretStoreVerifyArguments(arguments *api.SecretStoreVerifyRequest) error {
	if arguments.Timeout == 0 {
		arguments.Timeout = 45
	}
	if arguments.Timeout < 1 || arguments.Timeout > 120 {
		return fmt.Errorf("timeout must be between 1 and 120 seconds")
	}
	if arguments.Tier == "" {
		arguments.Tier = "release"
	}
	if arguments.Tier != "release" && arguments.Tier != "ci" {
		return fmt.Errorf("tier must be release or ci")
	}
	if arguments.Repo != "" {
		if !arguments.ReleaseTier {
			return fmt.Errorf("repo requires release_tier")
		}
		parts := strings.Split(arguments.Repo, "/")
		if len(parts) != 3 {
			return fmt.Errorf("repo must be upstream/org/repo")
		}
		for _, part := range parts {
			if err := gitcache.ValidateSegment("identity", part); err != nil {
				return err
			}
		}
	}
	if arguments.Tier == "ci" && (!arguments.ReleaseTier || arguments.Repo == "") {
		return fmt.Errorf("ci tier requires release_tier and repo")
	}
	if arguments.ReleaseTier && (arguments.Keys || len(arguments.Expect) != 0) {
		return fmt.Errorf("keys and expect are not supported with release_tier")
	}
	if len(arguments.Paths) > 32 || len(arguments.Expect) > 32 {
		return fmt.Errorf("paths and expect each allow at most 32 entries")
	}
	for _, path := range arguments.Paths {
		if _, _, err := periapsis.ParseUpstreamSecretStorePath(path); err != nil {
			return err
		}
	}
	for _, expect := range arguments.Expect {
		base, fields, ok := strings.Cut(expect, "/")
		if !ok || len(expect) > 4096 || strings.ContainsAny(expect, "\x00\r\n") || base == "" || fields == "" {
			return fmt.Errorf("expect must be <path-base>/<field>[,<field>,...] (maximum 4096 bytes)")
		}
		for _, field := range strings.Split(fields, ",") {
			if strings.TrimSpace(field) == "" {
				return fmt.Errorf("expect contains an empty field name")
			}
		}
	}
	return nil
}
