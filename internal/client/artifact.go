package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ArtifactSHA256Header is the digest header raw artifact downloads carry. It
// must equal api.ArtifactSHA256Header; a test pins the two together.
const ArtifactSHA256Header = "X-Oberth-Artifact-SHA256"

// ErrArtifactIntegrity marks a download whose received bytes differ from what
// the server declared: a short or long body, or a digest mismatch (#1011).
var ErrArtifactIntegrity = errors.New("artifact download failed verification")

// ArtifactReceipt describes the bytes a download actually delivered.
type ArtifactReceipt struct {
	Bytes  int64
	SHA256 string
	// Verified is true when the server declared a digest and the received
	// bytes matched it. A server that predates the digest header yields an
	// unverified receipt; callers decide whether that is acceptable.
	Verified bool
}

// DownloadArtifact streams one raw artifact into output while hashing it, and
// fails unless the bytes received match the declared Content-Length and
// digest. Output may already hold a prefix when it fails; callers writing a
// file should write to a temporary path and rename only on success.
func (client *Client) DownloadArtifact(ctx context.Context, path string, output io.Writer) (ArtifactReceipt, error) {
	request, err := client.newGetRequest(ctx, path, nil)
	if err != nil {
		return ArtifactReceipt{}, err
	}
	response, err := client.download.Do(request)
	if err != nil {
		return ArtifactReceipt{}, classifyTransport(path, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return ArtifactReceipt{}, statusError(path, response)
	}
	receipt, err := ReceiveArtifact(response, output)
	if err != nil {
		return receipt, fmt.Errorf("client: %s: %w", path, err)
	}
	return receipt, nil
}

// ReceiveArtifact copies a 200 artifact response body into output and checks
// it against the response's Content-Length and ArtifactSHA256Header. It is
// shared by the REST client and the MCP command's save-to-file path.
func ReceiveArtifact(response *http.Response, output io.Writer) (ArtifactReceipt, error) {
	declared := response.Header.Get(ArtifactSHA256Header)
	if declared != "" && !validSHA256Hex(declared) {
		return ArtifactReceipt{}, fmt.Errorf("%w: malformed %s header", ErrArtifactIntegrity, ArtifactSHA256Header)
	}
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(output, hash), response.Body)
	receipt := ArtifactReceipt{Bytes: count, SHA256: hex.EncodeToString(hash.Sum(nil))}
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return receipt, fmt.Errorf("%w: connection ended after %d of %d bytes", ErrArtifactIntegrity, count, response.ContentLength)
		}
		return receipt, fmt.Errorf("read artifact after %d bytes: %w", count, err)
	}
	if response.ContentLength >= 0 && count != response.ContentLength {
		return receipt, fmt.Errorf("%w: received %d bytes, server declared %d", ErrArtifactIntegrity, count, response.ContentLength)
	}
	if declared == "" {
		return receipt, nil
	}
	if receipt.SHA256 != declared {
		return receipt, fmt.Errorf("%w: sha256 %s, server declared %s", ErrArtifactIntegrity, receipt.SHA256, declared)
	}
	receipt.Verified = true
	return receipt, nil
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
