package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

// firstAdminConfig binds the private exec to its daemon and public trust files.
// Production paths are fixed; tests use a real store and gate on private paths.
type firstAdminConfig struct {
	DatabasePath      string
	SocketPath        string
	StoreOptions      store.Options
	Authority         *clientcert.Authority
	ServerCA          string
	ServerCertificate string
}

func loadFirstAdminConfig(ctx context.Context) (firstAdminConfig, error) {
	authority, err := loadClientAuthority()
	if err != nil {
		return firstAdminConfig{}, err
	}
	var ca, certificate bytes.Buffer
	if err := adminReadPublicFiles(ctx, []string{"/run/oberth-identities/server/ca.crt", "/run/oberth-identities/server/tls.crt", "/etc/oberth/tls/tls.crt"}, &ca, adminPublicFiles); err != nil {
		return firstAdminConfig{}, err
	}
	if err := adminReadPublicFiles(ctx, []string{"/run/oberth-identities/server/tls.crt", "/etc/oberth/tls/tls.crt"}, &certificate, adminPublicFiles); err != nil {
		return firstAdminConfig{}, err
	}
	return firstAdminConfig{DatabasePath: "/data/oberth.sqlite", SocketPath: defaultAdminSocket, StoreOptions: configuredStoreOptions(ctx), Authority: authority, ServerCA: ca.String(), ServerCertificate: certificate.String()}, nil
}

// runCreateFirstAdmin is the sole installation-time server exec. Public CSRs
// enter on stdin; private client keys remain on their originating machines.
func runCreateFirstAdmin(ctx context.Context, input io.Reader, output io.Writer, config firstAdminConfig) error {
	body, err := io.ReadAll(io.LimitReader(input, api.AdminRequestBytes+1))
	if err != nil || api.ValidateAdminObject(body) != nil {
		return errors.New("first admin requires one bounded enrollment object")
	}
	var request api.FirstAdminRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return errors.New("invalid first-admin enrollment")
	}
	if err := app.ValidateUplinkIdentity(request.Identity); err != nil {
		return err
	}
	fingerprint, err := app.AuthorizedKeyFingerprintReader(bytes.NewBufferString(request.SSHPublicKey))
	if err != nil {
		return err
	}
	authority := config.Authority
	if authority == nil {
		return errors.New("client CA unavailable")
	}
	_, installerLeaf, err := authority.Issue(request.Identity, []byte(request.InstallerCSR), time.Hour)
	if err != nil {
		return errors.New("invalid installer CSR")
	}
	if request.CSR != "" {
		if _, _, err := authority.Issue(request.Identity, []byte(request.CSR), 0); err != nil {
			return errors.New("invalid first-admin CSR")
		}
	}
	// Each daemon gate takes a writer fence, so every request must finish
	// before CreateFirstAdmin begins its immediate writer transaction.
	for _, operation := range []string{"uplink.database.open", "uplink.token.create", "uplink.register", "uplink.token.activate", "uplink.cert.issue"} {
		if err := requestAdminMutationGate(ctx, config.SocketPath, operation, config.DatabasePath); err != nil {
			return err
		}
	}
	database, err := store.OpenAdminClient(ctx, config.DatabasePath, config.StoreOptions)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	policy, err := requestUplinkTokenPolicy(ctx, config.SocketPath, config.DatabasePath)
	if err != nil {
		return err
	}
	result := api.FirstAdminResult{ServerCA: config.ServerCA, ServerCertificate: config.ServerCertificate}
	err = database.CreateFirstAdmin(ctx, administrativeActor, request.Identity, fingerprint, clientcert.Pin(installerLeaf), request.CSR == "" || request.Bearer, policy, func(enrollment *store.FirstAdminEnrollment) error {
		var token bytes.Buffer
		defer func() { clear(token.Bytes()) }()
		issuer := auth.Issuer{Tokens: enrollment, Bind: func(ctx context.Context, pending model.TokenCredential) error {
			return enrollment.Register(ctx, pending)
		}}
		if _, err := issuer.Issue(ctx, request.Identity, &token); err != nil {
			return err
		}
		if request.CSR == "" || request.Bearer {
			result.Token = strings.TrimSpace(token.String())
		}
		certIssuer := &service.ClientCertIssuer{Authority: authority, Store: enrollment}
		if request.CSR != "" {
			result.Certificate, err = service.IssueClientCert(ctx, certIssuer, administrativeActor, request.Identity, fingerprint, []byte(request.CSR), 0)
			if err != nil {
				return err
			}
		}
		result.InstallerCertificate, err = service.IssueClientCert(ctx, certIssuer, administrativeActor, request.Identity, fingerprint, []byte(request.InstallerCSR), time.Hour)
		return err
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
