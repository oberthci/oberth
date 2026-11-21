package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
)

type installationSecretWriter struct {
	remote *client.Client
	out    io.Writer
}

func (w installationSecretWriter) PutSecret(ctx context.Context, r installer.SecretPutRequest, value []byte) error {
	var grant apicontract.Grant
	if err := w.remote.Invoke(ctx, "access_allow", apicontract.AccessAllowArgs{Repo: r.Repo, Workflow: r.File, Step: r.Step, Secret: r.Path}, &grant); err != nil {
		message := "Secret access not granted; secret not stored.\n"
		if client.IsOutcomeUnknown(err) {
			message = "Secret access outcome is unknown; secret was not sent.\n"
		}
		_, outputErr := io.WriteString(w.out, message)
		return errors.Join(err, outputErr)
	}
	// A saved grant stores the value; its schedule follow-up is its own line.
	granted := "Secret access granted.\n"
	if grant.Warning != "" {
		granted += grant.Warning + "\n"
	}
	if _, err := io.WriteString(w.out, granted); err != nil {
		return err
	}
	_, err := w.remote.PutSecret(ctx, apicontract.SecretPutArgs{Repo: r.Repo, Workflow: r.File, Step: r.Step, Secret: r.Path}, value)
	if err != nil {
		message := "Secret not stored.\n"
		if client.IsOutcomeUnknown(err) {
			message = "Secret storage outcome is unknown.\n"
			err = secretWriteGuidance(err)
		}
		_, outputErr := io.WriteString(w.out, message)
		return errors.Join(err, outputErr)
	}
	return nil
}
func (a application) runSecretPut(ctx context.Context, r installationRequest, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if a.env == nil {
		return installationProblem("No installation profile is selected.", "Select an administrator profile, then retry.")
	}
	options, err := client.Resolve(client.Options{}, a.base, a.env)
	if err != nil {
		return err
	}
	cfg := a.config
	cfg.Options = options
	remote, err := client.New(cfg)
	if err != nil {
		return err
	}
	defer remote.Close()
	request := installer.SecretPutRequest{Repo: r.text("repo"), File: r.text("file"), Step: r.text("step"), Path: r.args[0]}
	if err = installer.SecretPut(ctx, installationSecretWriter{remote: remote, out: out}, request, in); err != nil {
		return err
	}
	_, err = io.WriteString(out, "Secret stored for the approved step.\n")
	return err
}

func secretWriteGuidance(err error) error {
	var problem *publicerr.Error
	if !errors.As(err, &problem) {
		return err
	}
	guidance := publicerr.New(problem.Code(), problem.Reason(), "Check the request's audit entry with oberth audit; putting the same value again is safe.", err)
	guidance.ID, guidance.Context, guidance.Detail = problem.ID, slices.Clone(problem.Context), problem.Detail
	return guidance
}
