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
	_, installerLeaf, err := authority.Issue(request.Identity, []byte(request.InstallerCSR), time.Hour)
	if err != nil {
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
	policy, err := requestLiveUplinkTokenPolicy(ctx, "/data/oberth.sqlite")
	if err != nil {
		return err
	}
	result := api.FirstAdminResult{ServerCA: publicCA.String(), ServerCertificate: publicCertificate.String()}
	err = database.CreateFirstAdmin(ctx, administrativeActor, request.Identity, fingerprint, clientcert.Pin(installerLeaf), request.CSR == "" || request.Bearer, policy, func(enrollment *store.FirstAdminEnrollment) error {
		var token bytes.Buffer
		defer func() { clear(token.Bytes()) }()
		issuer := auth.Issuer{Tokens: gatedAdminTokens{FirstAdminEnrollment: enrollment, Gate: gate}, Bind: func(ctx context.Context, pending model.TokenCredential) error {
			if err := gate(ctx, "uplink.register"); err != nil {
				return err
			}
			return enrollment.Register(ctx, pending)
		}}
		if _, err := issuer.Issue(ctx, request.Identity, &token); err != nil {
			return err
		}
		if request.CSR == "" || request.Bearer {
			result.Token = strings.TrimSpace(token.String())
		}
		if err := gate(ctx, "uplink.cert.issue"); err != nil {
			return err
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
