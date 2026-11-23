package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/oauth2/google/externalaccount"
)

const registryHost = "europe-west4-docker.pkg.dev"

const federationFailed refusal = "Registry federation failed."

// The cloud endpoints are fixed. Authored configuration selects only a provider
// and account, never a destination to which the projected identity can be sent.
func registryLogin(ctx context.Context, args []string) error {
	config, dir, err := registryIdentity(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	source, err := externalaccount.NewTokenSource(ctx, config)
	if err != nil {
		return federationFailed
	}
	access, err := source.Token()
	if err != nil || !access.Valid() {
		// Provider errors can echo credential material; never print or wrap them.
		return federationFailed
	}
	return writeRegistryLogin(dir, access.AccessToken)
}

func registryIdentity(args []string) (externalaccount.Config, string, error) {
	f := flag.NewFlagSet("registry-login", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	provider := f.String("provider", "", "workload identity provider resource")
	account := f.String("service-account", "", "registry writer account email")
	subject := f.String("identity-file", "", "projected identity file")
	dir := f.String("dir", "", "new private registry configuration directory")
	err := f.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		f.SetOutput(os.Stdout)
		f.Usage()
		return externalaccount.Config{}, "", err
	}
	if err != nil || f.NArg() != 0 || *subject == "" || *dir == "" ||
		!regexp.MustCompile(`^projects/[0-9]+/locations/global/workloadIdentityPools/[a-z0-9-]+/providers/[a-z0-9-]+$`).MatchString(*provider) ||
		!regexp.MustCompile(`^[a-z0-9-]+@[a-z0-9-]+\.iam\.gserviceaccount\.com$`).MatchString(*account) {
		return externalaccount.Config{}, "", federationFailed
	}
	return externalaccount.Config{
		Audience:                       "//iam.googleapis.com/" + *provider,
		SubjectTokenType:               "urn:ietf:params:oauth:token-type:jwt",
		TokenURL:                       "https://sts.googleapis.com/v1/token",
		ServiceAccountImpersonationURL: "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + *account + ":generateAccessToken",
		ServiceAccountImpersonationLifetimeSeconds: 3600,
		Scopes:           []string{"https://www.googleapis.com/auth/cloud-platform"},
		CredentialSource: &externalaccount.CredentialSource{File: *subject},
	}, *dir, nil
}

func writeRegistryLogin(dir, access string) error {
	config := map[string]any{"auths": map[string]any{registryHost: map[string]string{
		"auth": base64.StdEncoding.EncodeToString([]byte("oauth2accesstoken:" + access)),
	}}}
	body, err := json.Marshal(config)
	if err != nil {
		return federationFailed
	}
	// Refuse existing directories, including links, rather than using stale login.
	if err := os.Mkdir(dir, 0700); err != nil {
		return federationFailed
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), body, 0600); err != nil {
		return federationFailed
	}
	return nil
}
