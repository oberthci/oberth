package main

import (
	"encoding/json"
	"path/filepath"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
)

func (s *flowSession) checkJoinCheckout(invitation model.InvitationFile) error {
	if err := s.refuseBareCheckout(); err != nil {
		return err
	}
	repo, err := s.git("config", "--get", "oberth.repository")
	if err != nil || repo == "" {
		return nil
	}
	if err = s.checkGitBinding(repo, "join"); err != nil {
		return err
	}
	dir, err := s.git("config", "--local", "--get", "oberth.profile")
	if err != nil || !filepath.IsAbs(dir) {
		return flowError("join_binding", err)
	}
	return checkJoinInstallation(dir, invitation)
}

func checkJoinInstallation(dir string, invitation model.InvitationFile) error {
	raw, err := client.ReadPublic(filepath.Join(dir, "installation.json"))
	var profile client.Profile
	if err != nil || json.Unmarshal(raw, &profile) != nil || !apicontract.SameServerAddress(profile.ServerURL, invitation.ServerURL) {
		return flowError("join_binding", err)
	}
	caFile := profile.CAFile
	if caFile == "" {
		caFile = filepath.Join(dir, "ca.crt")
	}
	raw, err = client.ReadPublic(caFile)
	if err != nil {
		return flowError("join_binding", err)
	}
	ca, err := clientcert.ParseCertificate(raw)
	if err != nil || clientcert.Pin(ca) != invitation.CAPin {
		return flowError("join_binding", err)
	}
	return nil
}

func (s *flowSession) refuseBareCheckout() error {
	bare, err := s.git("rev-parse", "--is-bare-repository")
	if err == nil && bare == "true" {
		return flowError("bare_checkout", nil)
	}
	return nil
}
