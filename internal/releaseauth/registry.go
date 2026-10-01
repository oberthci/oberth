// Package releaseauth exchanges memory-only delegated credentials for the
// fixed, least-privileged principals that publish Oberth release artifacts.
package releaseauth

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

const registryScope = "https://www.googleapis.com/auth/cloud-platform"

var sourceEmail = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]@skipopsmain\.iam\.gserviceaccount\.com$`)

// RegistryPrincipal closes role selection before any credential is used.
func RegistryPrincipal(action string) (string, error) {
	var name string
	switch action {
	case "publish-images":
		name = "oberth-release-publisher"
	case "publish-chart":
		name = "oberth-chart-publisher"
	case "verify":
		name = "oberth-release-reader"
	default:
		return "", errors.New("unsupported registry role")
	}
	return name + "@skipopsmain.iam.gserviceaccount.com", nil
}

// RegistryToken uses the supplied source key only to request an access token
// for the selected principal. IAM must grant that source TokenCreator on only
// its matching target; a different role fails at Google's authority boundary.
// Key-provided token endpoints, ambient ADC and environment credentials are
// never consulted. Errors deliberately omit provider bodies and credentials.
func RegistryToken(ctx context.Context, action string, keyJSON []byte) ([]byte, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return registryToken(ctx, action, keyJSON, client, "https://oauth2.googleapis.com/token", "https://iamcredentials.googleapis.com/v1")
}

func registryToken(ctx context.Context, action string, keyJSON []byte, client *http.Client, tokenURL, iamURL string) ([]byte, error) {
	principal, err := RegistryPrincipal(action)
	if err != nil {
		return nil, err
	}
	if len(keyJSON) == 0 || len(keyJSON) > 64<<10 {
		return nil, errors.New("invalid registry source credential")
	}
	// Duplicate keys are refused instead of permitting parser disagreement
	// between the credential validator and JWT signer.
	decoder := json.NewDecoder(bytes.NewReader(keyJSON))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("invalid registry source credential")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		name, valid := key.(string)
		if err != nil || !valid || fields[name] != nil {
			return nil, errors.New("invalid registry source credential")
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errors.New("invalid registry source credential")
		}
		fields[name] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, errors.New("invalid registry source credential")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid registry source credential")
	}
	field := func(name string) string {
		var result string
		_ = json.Unmarshal(fields[name], &result)
		return result
	}
	email, privateKey := field("client_email"), field("private_key")
	if field("type") != "service_account" || field("project_id") != "skipopsmain" || !sourceEmail.MatchString(email) || privateKey == "" {
		return nil, errors.New("invalid registry source credential")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	config := jwt.Config{Email: email, PrivateKey: []byte(privateKey), PrivateKeyID: field("private_key_id"), Scopes: []string{registryScope}, TokenURL: tokenURL}
	defer clear(config.PrivateKey)
	source, err := config.TokenSource(ctx).Token()
	if err != nil {
		return nil, errors.New("registry source authentication failed")
	}
	body := `{"scope":["` + registryScope + `"],"lifetime":"3600s"}`
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, iamURL+"/projects/-/serviceAccounts/"+principal+":generateAccessToken", strings.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid registry delegation request")
	}
	request.Header.Set("Content-Type", "application/json")
	source.SetAuthHeader(request)
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("registry delegation request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("registry role delegation refused")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("invalid registry delegation response")
	}
	defer clear(data)
	var result struct {
		AccessToken string    `json:"accessToken"`
		ExpireTime  time.Time `json:"expireTime"`
	}
	if json.Unmarshal(data, &result) != nil || result.AccessToken == "" || len(result.AccessToken) > 16384 || strings.ContainsAny(result.AccessToken, "\r\n\t ") || time.Until(result.ExpireTime) < 5*time.Minute || time.Until(result.ExpireTime) > 65*time.Minute {
		return nil, errors.New("invalid registry delegation response")
	}
	return []byte(result.AccessToken), nil
}
