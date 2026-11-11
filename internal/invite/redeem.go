package invite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
)

type Certificate struct {
	ServerCA       string   `json:"-"`
	Identity       string   `json:"identity"`
	Scope          []string `json:"scope"`
	SSHFingerprint string   `json:"ssh_fingerprint"`
	Certificate    string   `json:"certificate"`
}

// PinnedClient verifies the presented server chain and hostname against the
// exact CA DER digest from the invitation before any HTTP body can be sent.
func PinnedClient(address, pin string) (*http.Client, error) {
	if err := ValidateURL(address); err != nil {
		return nil, err
	}
	if err := ValidatePin(pin); err != nil {
		return nil, err
	}
	parsed, _ := url.Parse(address)
	configuration := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: parsed.Hostname(),
		InsecureSkipVerify: true, //nolint:gosec // G402: VerifyConnection performs full hostname/chain validation anchored exclusively on the supplied CA digest.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) < 2 {
				return errors.New("invite CA was not presented")
			}
			roots := x509.NewCertPool()
			intermediates := x509.NewCertPool()
			matched := false
			for _, cert := range state.PeerCertificates[1:] {
				digest := sha256.Sum256(cert.Raw)
				if cert.IsCA && cert.BasicConstraintsValid && hex.EncodeToString(digest[:]) == pin {
					roots.AddCert(cert)
					matched = true
				} else {
					intermediates.AddCert(cert)
				}
			}
			if !matched {
				return errors.New("invite CA pin does not match the server")
			}
			_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{DNSName: parsed.Hostname(), Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			return err
		},
	}
	transport := &http.Transport{TLSClientConfig: configuration, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func Redeem(ctx context.Context, file File, csr, sshKey string) (Certificate, error) {
	var result Certificate
	if _, err := SecretDigest(file.Secret); err != nil {
		return result, err
	}
	if !file.ExpiresAt.After(time.Now()) {
		return result, errors.New("invite has expired")
	}
	if _, err := clientcert.ParseCSR([]byte(csr)); err != nil {
		return result, errors.New("invalid local client CSR")
	}
	sshFingerprint, err := SSHFingerprint([]byte(sshKey))
	if err != nil {
		return result, err
	}
	client, err := PinnedClient(file.ServerURL, file.CAPin)
	if err != nil {
		return result, err
	}
	defer client.CloseIdleConnections()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "uplink_redeem", "arguments": map[string]string{"secret": file.Secret, "server_url": file.ServerURL, "ca_pin": file.CAPin, "csr": csr, "ssh_public_key": sshKey}}})
	if err != nil {
		return result, errors.New("cannot encode invite redemption")
	}
	defer clear(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, file.ServerURL+"/enroll", bytes.NewReader(body))
	if err != nil {
		return result, errors.New("invalid invite server URL")
	}
	request.GetBody = nil // A consumed invite must never be replayed by the transport.
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("MCP-Protocol-Version", "2025-06-18")
	response, err := client.Do(request)
	if err != nil {
		return result, errors.New("invite connection failed; verify the server address, CA pin and certificate")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return result, errors.New("invite redemption was refused; the credential was not retried")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (96<<10)+1))
	if err != nil || len(raw) > 96<<10 {
		return result, errors.New("invalid invite response")
	}
	var reply struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Error   json.RawMessage `json:"error"`
		Result  struct {
			IsError    bool        `json:"isError"`
			Structured Certificate `json:"structuredContent"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.JSONRPC != "2.0" || reply.ID != 1 || len(reply.Error) != 0 || reply.Result.IsError || reply.Result.Structured.Certificate == "" || reply.Result.Structured.Identity != file.Identity || reply.Result.Structured.SSHFingerprint != sshFingerprint {
		return result, errors.New("invite redemption was refused or returned an invalid certificate")
	}
	result = reply.Result.Structured
	for _, cert := range response.TLS.PeerCertificates[1:] {
		digest := sha256.Sum256(cert.Raw)
		if cert.IsCA && hex.EncodeToString(digest[:]) == file.CAPin {
			result.ServerCA = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
			break
		}
	}
	if result.ServerCA == "" {
		return Certificate{}, errors.New("invite response omitted the pinned CA")
	}
	return result, nil
}
