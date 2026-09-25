package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/oberthci/oberth/internal/installer"
)

func runUpgrade(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var cfg installer.UpgradeConfig
	var kubeContext string
	flags.StringVar(&cfg.Namespace, "namespace", "", "Oberth namespace (default: oberth)")
	flags.StringVar(&kubeContext, "context", "", "kubeconfig context to target (default: current-context)")
	flags.BoolVar(&cfg.DryRun, "dry-run", false, "show what would change without doing it")
	flags.BoolVar(&cfg.Yes, "yes", false, "proceed without confirmation for non-local targets")
	flags.StringVar(&cfg.ChartOverride, "chart", "", "override the chart reference (for testing)")
	timeout := flags.Duration("timeout", 5*time.Minute, "wait timeout for rollout completion")

	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: upgrade accepts flags only, no positional arguments", errUsage)
	}

	cfg.Timeout = *timeout
	cfg.BinaryVersion = version

	// Validate early so dev-build rejection does not require a cluster.
	if err := cfg.ValidateUpgrade(); err != nil {
		return err
	}

	kubeClient, restConfig, selectedContext, err := installer.LoadKubeConfigForContext(kubeContext, output)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	// Resolve helm kubeconfig targeting so helm upgrade targets the same
	// cluster the binary resolved (#464).
	helmKubeArgs, _, _ := installer.ResolveHelmKubeArgs(kubeContext)
	runHelm := installer.HelmWithKubeArgs(installer.DefaultRunHelm, helmKubeArgs)

	_, err = installer.RunUpgrade(ctx, cfg, installer.Deps{
		Output:      output,
		RunHelm:     runHelm,
		RunCommand:  installer.DefaultRunCommand,
		KubeClient:  kubeClient,
		RestConfig:  restConfig,
		ContextName: selectedContext,
	})
	return err
}
