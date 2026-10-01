package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

const r2CredentialLifetime = int64(1800)

func r2HTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func readR2Token(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("read R2 parent API token")
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("R2 parent token is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(raw) > 4096 {
		clear(raw)
		return nil, errors.New("R2 parent token exceeds bound or cannot be read")
	}
	return raw, nil
}

// AI-CONTRACT: Bucket item tokens are S3-only. Derive the documented temporary
// credential locally; never widen the parent grant or fall back to its permanent
// S3 credentials. R2 enforces this JWT's bucket, prefix, scope and expiry.
// https://developers.cloudflare.com/r2/examples/authenticate-r2-temp-credentials/
func localR2Credentials(token, parentID, account, bucket, prefix string, now time.Time) (credentials, error) {
	issued := now.Unix()
	if len(token) > 4096 || !tokenPattern.MatchString(token) || !accountPattern.MatchString(parentID) ||
		!accountPattern.MatchString(account) || !bucketPattern.MatchString(bucket) ||
		!prefixPattern.MatchString(prefix) || strings.Contains(prefix, "..") ||
		issued <= 0 || issued > math.MaxInt64-r2CredentialLifetime {
		return credentials{}, errors.New("invalid R2 temporary credential scope or time")
	}
	claims := struct {
		Bucket string `json:"bucket"`
		Scope  string `json:"scope"`
		Paths  struct {
			PrefixPaths []string `json:"prefixPaths"`
			ObjectPaths []string `json:"objectPaths"`
		} `json:"paths"`
		Subject   string `json:"sub"`
		Issuer    string `json:"iss"`
		Audience  string `json:"aud"`
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
	}{Bucket: bucket, Scope: "object-read-write", Subject: account, Issuer: parentID,
		Audience: account + ".r2.cloudflarestorage.com", IssuedAt: issued, ExpiresAt: issued + r2CredentialLifetime}
	claims.Paths.PrefixPaths = []string{prefix}
	claims.Paths.ObjectPaths = []string{}
	payload, err := json.Marshal(claims)
	if err != nil {
		return credentials{}, errors.New("encode R2 temporary credential scope")
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	parentDigest := sha256.Sum256([]byte(token))
	parentSecret := make([]byte, hex.EncodedLen(len(parentDigest)))
	hex.Encode(parentSecret, parentDigest[:])
	defer clear(parentSecret)
	defer clear(parentDigest[:])
	mac := hmac.New(sha256.New, parentSecret)
	_, _ = mac.Write([]byte(unsigned))
	jwt := unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	temporaryDigest := sha256.Sum256([]byte(jwt))
	return credentials{parentID, hex.EncodeToString(temporaryDigest[:]), base64.StdEncoding.EncodeToString([]byte("jwt/" + jwt))}, nil
}
