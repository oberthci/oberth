// Package auth issues and authenticates Oberth bearer credentials without ever
// persisting their plaintext form.
package auth

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

var ErrInvalidToken = errors.New("auth: invalid token")

type TokenRepository interface {
	CreateTokenCredential(context.Context, model.TokenCredentialSpec) (model.TokenCredential, error)
	ActivateTokenCredential(context.Context, string) (model.TokenCredential, error)
	AuthenticatedUplinkByDigest(context.Context, []byte) (model.AuthenticatedUplink, error)
	TouchTokenCredential(context.Context, string) error
}

type Issuer struct {
	Tokens TokenRepository
	Random io.Reader
	// Bind durably associates the pending credential with exactly one uplink.
	// It runs only after the complete plaintext write and before activation.
	Bind func(context.Context, model.TokenCredential) error
}

// Issue persists only the token's SHA-256 digest in an inactive state, emits
// its plaintext in exactly one write, and activates it only after that write
// completes. The plaintext is never returned from this API.
func (i Issuer) Issue(ctx context.Context, name string, output io.Writer) (model.TokenCredential, error) {
	if i.Tokens == nil || i.Bind == nil || output == nil || strings.TrimSpace(name) == "" {
		return model.TokenCredential{}, errors.New("auth: token repository, uplink binder, name, and output are required")
	}
	random := i.Random
	if random == nil {
		random = cryptorand.Reader
	}
	var secret [32]byte
	if _, err := io.ReadFull(random, secret[:]); err != nil {
		return model.TokenCredential{}, fmt.Errorf("auth: generate token: %w", err)
	}
	plaintext := "oberth_" + base64.RawURLEncoding.EncodeToString(secret[:])
	digest := sha256.Sum256([]byte(plaintext))
	credential, err := i.Tokens.CreateTokenCredential(ctx, model.TokenCredentialSpec{
		Name: name, Digest: digest[:],
	})
	if err != nil {
		return model.TokenCredential{}, fmt.Errorf("auth: store token digest: %w", err)
	}

	body := []byte(plaintext + "\n")
	written, writeErr := output.Write(body)
	if writeErr != nil || written != len(body) {
		if writeErr != nil {
			return model.TokenCredential{}, fmt.Errorf("auth: emit token: %w", writeErr)
		}
		return model.TokenCredential{}, io.ErrShortWrite
	}
	if err := i.Bind(ctx, credential); err != nil {
		return model.TokenCredential{}, fmt.Errorf("auth: bind emitted token to uplink: %w", err)
	}
	credential, err = i.Tokens.ActivateTokenCredential(ctx, credential.ID)
	if err != nil {
		return model.TokenCredential{}, fmt.Errorf("auth: activate emitted token: %w", err)
	}
	return credential, nil
}

type Authenticator struct {
	Tokens TokenRepository
}

func (a Authenticator) Authenticate(ctx context.Context, plaintext string) (model.AuthenticatedUplink, error) {
	if a.Tokens == nil || plaintext == "" {
		return model.AuthenticatedUplink{}, ErrInvalidToken
	}
	digest := sha256.Sum256([]byte(plaintext))
	authenticated, err := a.Tokens.AuthenticatedUplinkByDigest(ctx, digest[:])
	if errors.Is(err, store.ErrNotFound) {
		return model.AuthenticatedUplink{}, ErrInvalidToken
	}
	if err != nil {
		return model.AuthenticatedUplink{}, fmt.Errorf("auth: look up token uplink: %w", err)
	}
	credential := authenticated.TokenCredential
	if credential.RevokedAt != nil || len(credential.Digest) != sha256.Size ||
		subtle.ConstantTimeCompare(credential.Digest, digest[:]) != 1 {
		return model.AuthenticatedUplink{}, ErrInvalidToken
	}
	if err := a.Tokens.TouchTokenCredential(ctx, credential.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return model.AuthenticatedUplink{}, ErrInvalidToken
		}
		return model.AuthenticatedUplink{}, fmt.Errorf("auth: record token use: %w", err)
	}
	return authenticated, nil
}
