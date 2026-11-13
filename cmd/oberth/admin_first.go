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
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// runCreateFirstAdmin is the sole installation-time server exec. Public CSRs
// enter on stdin; private client keys remain on their originating machines.
func runCreateFirstAdmin(ctx context.Context, input io.Reader, output io.Writer) error {
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
	authority, err := loadClientAuthority()
	if err != nil {
		return err
	}
	if authority == nil {
		return errors.New("client CA unavailable")
	}
	if _, _, err := authority.Issue(request.Identity, []byte(request.InstallerCSR), time.Hour); err != nil {
		return errors.New("invalid installer CSR")
	}
	if request.CSR != "" {
		if _, _, err := authority.Issue(request.Identity, []byte(request.CSR), 0); err != nil {
			return errors.New("invalid first-admin CSR")
		}
	}
	var publicCA, publicCertificate bytes.Buffer
	if err := adminReadPublicFiles(ctx, []string{"/run/oberth-identities/server/ca.crt", "/run/oberth-identities/server/tls.crt", "/etc/oberth/tls/tls.crt"}, &publicCA, adminPublicFiles); err != nil {
		return err
	}
	if err := adminReadPublicFiles(ctx, []string{"/run/oberth-identities/server/tls.crt", "/etc/oberth/tls/tls.crt"}, &publicCertificate, adminPublicFiles); err != nil {
		return err
	}
	gate := func(ctx context.Context, operation string) error {
		return requestLiveAdminMutationGate(ctx, operation, "/data/oberth.sqlite")
	}
	if err := gate(ctx, "uplink.database.open"); err != nil {
		return err
	}
	database, err := store.OpenAdminClient(ctx, "/data/oberth.sqlite", configuredStoreOptions(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	uplinks, err := database.ListUplinkStatuses(ctx)
	if err != nil {
		return err
	}
	for _, uplink := range uplinks {
		if uplink.Admin {
			return errors.New("first admin already exists; use authenticated administrative APIs")
		}
	}
	result := api.FirstAdminResult{ServerCA: publicCA.String(), ServerCertificate: publicCertificate.String()}
	if request.CSR != "" && !request.Bearer {
		if err := gate(ctx, "uplink.register"); err != nil {
			return err
		}
		if _, err := database.RegisterCertificateUplink(ctx, administrativeActor, model.UplinkSpec{Identity: request.Identity, Fingerprint: fingerprint, Admin: true}); err != nil {
			return err
		}
	} else {
		policy, err := requestLiveUplinkTokenPolicy(ctx, "/data/oberth.sqlite")
		if err != nil {
			return err
		}
		tokens := gatedAdminTokens{Store: database, Gate: gate, Policy: policy}
		var token bytes.Buffer
		defer func() { clear(token.Bytes()) }()
		issuer := auth.Issuer{Tokens: tokens, Bind: func(ctx context.Context, pending model.TokenCredential) error {
			if err := gate(ctx, "uplink.register"); err != nil {
				return err
			}
			_, err := database.RegisterUplink(ctx, administrativeActor, model.UplinkSpec{Identity: request.Identity, Fingerprint: fingerprint, TokenCredentialID: pending.ID, Admin: true})
			return err
		}}
		if _, err := issuer.Issue(ctx, request.Identity, &token); err != nil {
			return err
		}
		result.Token = strings.TrimSpace(token.String())
	}
	if err := gate(ctx, "uplink.cert.issue"); err != nil {
		return err
	}
	if request.CSR != "" {
		result.Certificate, err = issueUplinkCertificate(ctx, database, request.Identity, fingerprint, []byte(request.CSR), 0)
		if err != nil {
			return err
		}
	}
	result.InstallerCertificate, err = issueUplinkCertificate(ctx, database, request.Identity, fingerprint, []byte(request.InstallerCSR), time.Hour)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
