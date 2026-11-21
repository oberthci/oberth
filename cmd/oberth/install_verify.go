package main

import (
	"context"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/kubeconfig"
)

func (s installationSession) verify(ctx context.Context) error {
	if err := installer.Verify(ctx, s.profile); err != nil {
		return err
	}
	target, err := kubeconfig.Open(s.app.env("KUBECONFIG"), s.profile.Context)
	if err != nil {
		return err
	}
	return installer.VerifyPipelineNamespace(ctx, target.Client.CoreV1().Namespaces(), s.profile.PipelineNamespace)
}
