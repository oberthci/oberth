// Package auditanchor obtains and verifies independently signed RFC 3161
// checkpoints for Oberth's local audit hash chain.
package auditanchor

import (
	"bytes"
	"context"
	"crypto"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/digitorus/pkcs7"
	"github.com/digitorus/timestamp"

	"github.com/oberthci/oberth/internal/model"
)

const (
	defaultHTTPTimeout     = 15 * time.Second
	defaultMaxResponseSize = 4 << 20
	defaultClockSkew       = 5 * time.Minute
)

var extendedKeyUsageOID = asn1.ObjectIdentifier{2, 5, 29, 37}

// ErrAuthorityUnavailable distinguishes transport/service outages, where a
// recently verified checkpoint may be used for the bounded fallback window,
// from malformed or cryptographically invalid responses, which fail closed.
var ErrAuthorityUnavailable = errors.New("audit anchor: timestamp authority unavailable")

type ClientConfig struct {
	Endpoint        string
	Roots           *x509.CertPool
	TLSCACert       *x509.CertPool // optional TLS trust anchors for the TSA HTTPS connection
	HTTPClient      *http.Client
	Now             func() time.Time
	MaxResponseSize int64
	ClockSkew       time.Duration
}

type Client struct {
	endpoint        string
	httpClient      *http.Client
	roots           *x509.CertPool
	now             func() time.Time
	maxResponseSize int64
	clockSkew       time.Duration
}

func NewClient(config ClientConfig) (*Client, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || strings.Contains(config.Endpoint, "#") || endpoint.Host == "" ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, errors.New("audit anchor: TSA endpoint must be an absolute HTTP(S) URL without credentials or a fragment")
	}
	if config.Roots == nil {
		return nil, errors.New("audit anchor: TSA trust roots are required")
	}
	if config.HTTPClient == nil {
		httpClient := &http.Client{Timeout: defaultHTTPTimeout}
		if config.TLSCACert != nil {
			httpClient.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS13,
					RootCAs:    config.TLSCACert,
				},
			}
		}
		config.HTTPClient = httpClient
	}
	httpClient := *config.HTTPClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("audit anchor: TSA redirects are not allowed")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MaxResponseSize <= 0 {
		config.MaxResponseSize = defaultMaxResponseSize
	}
	if config.ClockSkew <= 0 {
		config.ClockSkew = defaultClockSkew
	}
	return &Client{
		endpoint: endpoint.String(), httpClient: &httpClient, roots: config.Roots,
		now: config.Now, maxResponseSize: config.MaxResponseSize, clockSkew: config.ClockSkew,
	}, nil
}

func (client *Client) Timestamp(ctx context.Context, head model.AuditHead) (model.AuditAnchorSpec, error) {
	if head.ID < 0 || len(head.SHA256) != sha256.Size {
		return model.AuditAnchorSpec{}, errors.New("audit anchor: chain head is invalid")
	}
	nonce, err := cryptorand.Int(cryptorand.Reader, new(big.Int).Lsh(big.NewInt(1), 160))
	if err != nil {
		return model.AuditAnchorSpec{}, fmt.Errorf("audit anchor: generate request nonce: %w", err)
	}
	requestBody, err := (&timestamp.Request{
		HashAlgorithm: crypto.SHA256, HashedMessage: append([]byte(nil), head.SHA256...),
		Certificates: true, Nonce: nonce,
	}).Marshal()
	if err != nil {
		return model.AuditAnchorSpec{}, fmt.Errorf("audit anchor: encode request: %w", err)
	}
	startedAt := client.now().UTC()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return model.AuditAnchorSpec{}, fmt.Errorf("audit anchor: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/timestamp-query")
	request.Header.Set("Accept", "application/timestamp-reply")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return model.AuditAnchorSpec{}, fmt.Errorf("%w: request TSA: %w", ErrAuthorityUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
			return model.AuditAnchorSpec{}, fmt.Errorf("%w: TSA returned HTTP %d", ErrAuthorityUnavailable, response.StatusCode)
		}
		return model.AuditAnchorSpec{}, fmt.Errorf("audit anchor: TSA returned HTTP %d", response.StatusCode)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || !strings.EqualFold(mediaType, "application/timestamp-reply") {
		return model.AuditAnchorSpec{}, fmt.Errorf("audit anchor: TSA returned content type %q", response.Header.Get("Content-Type"))
	}
	receipt, err := io.ReadAll(io.LimitReader(response.Body, client.maxResponseSize+1))
	if err != nil {
		return model.AuditAnchorSpec{}, fmt.Errorf("%w: read TSA response: %w", ErrAuthorityUnavailable, err)
	}
	if int64(len(receipt)) > client.maxResponseSize {
		return model.AuditAnchorSpec{}, fmt.Errorf("audit anchor: TSA response exceeds %d bytes", client.maxResponseSize)
	}
	stamp, err := client.verifyReceipt(receipt, head.SHA256)
	if err != nil {
		return model.AuditAnchorSpec{}, err
	}
	if stamp.Nonce == nil || stamp.Nonce.Cmp(nonce) != 0 {
		return model.AuditAnchorSpec{}, errors.New("audit anchor: TSA response nonce does not match request")
	}
	finishedAt := client.now().UTC()
	if stamp.Time.Before(startedAt.Add(-client.clockSkew)) || stamp.Time.After(finishedAt.Add(client.clockSkew)) {
		return model.AuditAnchorSpec{}, fmt.Errorf("audit anchor: TSA time %s is outside the request window", stamp.Time.UTC().Format(time.RFC3339Nano))
	}
	return model.AuditAnchorSpec{
		AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...), TSAURL: client.endpoint,
		Receipt: append([]byte(nil), receipt...), AnchoredAt: stamp.Time.UTC(),
	}, nil
}

func (client *Client) Verify(anchor model.AuditAnchor) error {
	if anchor.AuditID < 0 || len(anchor.AuditSHA256) != sha256.Size || len(anchor.Receipt) == 0 || anchor.AnchoredAt.IsZero() {
		return errors.New("audit anchor: stored checkpoint is invalid")
	}
	stamp, err := client.verifyReceipt(anchor.Receipt, anchor.AuditSHA256)
	if err != nil {
		return err
	}
	if !stamp.Time.Equal(anchor.AnchoredAt) {
		return fmt.Errorf("audit anchor: signed time %s differs from stored time %s",
			stamp.Time.UTC().Format(time.RFC3339Nano), anchor.AnchoredAt.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

func (client *Client) verifyReceipt(receipt, expectedHash []byte) (*timestamp.Timestamp, error) {
	stamp, err := timestamp.ParseResponse(receipt)
	if err != nil {
		return nil, fmt.Errorf("audit anchor: parse TSA response: %w", err)
	}
	if stamp.HashAlgorithm != crypto.SHA256 || !bytes.Equal(stamp.HashedMessage, expectedHash) {
		return nil, errors.New("audit anchor: TSA response does not commit the requested SHA-256 head")
	}
	signedData, err := pkcs7.Parse(stamp.RawToken)
	if err != nil {
		return nil, fmt.Errorf("audit anchor: parse signed token: %w", err)
	}
	signer := signedData.GetOnlySigner()
	if signer == nil {
		return nil, errors.New("audit anchor: timestamp token must have exactly one signer")
	}
	if !slices.Contains(signer.ExtKeyUsage, x509.ExtKeyUsageTimeStamping) || !criticalExtendedKeyUsage(signer) {
		return nil, errors.New("audit anchor: signer certificate lacks a critical timestamping usage")
	}
	intermediates := x509.NewCertPool()
	for _, certificate := range signedData.Certificates {
		if !certificate.Equal(signer) {
			intermediates.AddCert(certificate)
		}
	}
	if err := signedData.VerifyWithOpts(x509.VerifyOptions{
		Roots: client.roots, Intermediates: intermediates, CurrentTime: stamp.Time,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
	}); err != nil {
		return nil, fmt.Errorf("audit anchor: verify TSA signature and trust chain: %w", err)
	}
	return stamp, nil
}

func criticalExtendedKeyUsage(certificate *x509.Certificate) bool {
	for _, extension := range certificate.Extensions {
		if extension.Id.Equal(extendedKeyUsageOID) {
			return extension.Critical
		}
	}
	return false
}
