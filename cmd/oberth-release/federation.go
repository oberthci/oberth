package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google/externalaccount"
)

const registryHost = "europe-west4-docker.pkg.dev"

const (
	federationConfig        refusal = "Registry federation config is invalid."
	federationExchange      refusal = "Registry identity exchange failed."
	federationImpersonation refusal = "Registry impersonation failed."
	federationDirectory     refusal = "Registry login directory refused."
)

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
	ctx, exchange := registryExchangeContext(ctx)
	source, err := externalaccount.NewTokenSource(ctx, config)
	if err != nil {
		return federationConfig
	}
	access, err := source.Token()
	if err != nil || !access.Valid() {
		// Provider errors can echo credential material; never print or wrap them.
		if exchange.impersonating.Load() {
			return federationImpersonation
		}
		return federationExchange
	}
	return writeRegistryLogin(dir, access.AccessToken)
}

// Track only the fixed request stage, never provider response bodies or errors.
type registryTransport struct {
	http.RoundTripper
	impersonating atomic.Bool
}

func (r *registryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host == "iamcredentials.googleapis.com" {
		r.impersonating.Store(true)
	}
	return r.RoundTripper.RoundTrip(request)
}

func registryExchangeContext(ctx context.Context) (context.Context, *registryTransport) {
	client := http.DefaultClient
	if configured, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		client = configured
	}
	copy := *client
	transport := &registryTransport{RoundTripper: copy.Transport}
	if transport.RoundTripper == nil {
		transport.RoundTripper = http.DefaultTransport
	}
	copy.Transport = transport
	return context.WithValue(ctx, oauth2.HTTPClient, &copy), transport
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
		return externalaccount.Config{}, "", federationConfig
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
		return federationDirectory
	}
	// Refuse existing directories, including links, rather than using stale login.
	if err := os.Mkdir(dir, 0700); err != nil {
		return federationDirectory
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), body, 0600); err != nil {
		return federationDirectory
	}
	return nil
}
