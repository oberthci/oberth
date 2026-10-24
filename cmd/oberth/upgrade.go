package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/store"
)

// upgradeStringSlice implements flag.Value for repeatable string flags.
type upgradeStringSlice []string

func (s *upgradeStringSlice) String() string { return strings.Join([]string(*s), ", ") }
func (s *upgradeStringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func runUpgrade(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	var cfg installer.UpgradeConfig
	var kubeContext string
	var userValues, userSet upgradeStringSlice
	flags.StringVar(&cfg.Namespace, "namespace", "", "Oberth namespace (default: oberth)")
	flags.StringVar(&kubeContext, "context", "", "kubeconfig context to target (default: current-context)")
	flags.StringVar(&cfg.ArgoControllerProfile, "argo-controller-profile", "", "explicitly reconcile the managed nonroot-static-v1 Argo profile and verify it before enabling server admission")
	flags.BoolVar(&cfg.WaitIdle, "wait-idle", false, "wait for all queued/running runs before upgrading (advisory, not an admission freeze)")
	flags.DurationVar(&cfg.WaitTimeout, "wait-timeout", installer.DefaultUpgradeWaitTimeout, "maximum queue wait, independent of --timeout")
	flags.StringVar(&cfg.WaitFor, "wait-for", "", "enable waiting for all or critical (tag/promotion/plan/apply/unknown) runs")
	flags.BoolVar(&cfg.DryRun, "dry-run", false, "show what would change without doing it")
	flags.BoolVar(&cfg.Yes, "yes", false, "proceed without confirmation for non-local targets")
	flags.StringVar(&cfg.ChartOverride, "chart", "", "override the chart reference (for testing)")
	flags.Var(&userValues, "values", "values file to pass to helm upgrade (repeatable)")
	flags.Var(&userValues, "f", "alias for --values")
	flags.Var(&userSet, "set", "one canonical dotted key=scalar per flag; escape literal commas (repeatable)")
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

	if cfg.WaitTimeout <= 0 {
		return fmt.Errorf("%w: --wait-timeout must be positive", errUsage)
	}
	waitForProvided, waitTimeoutProvided := false, false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "wait-for" {
			cfg.WaitIdle = true
			waitForProvided = true
		}
		if f.Name == "wait-timeout" {
			waitTimeoutProvided = true
		}
	})
	if waitTimeoutProvided && !cfg.WaitIdle {
		return fmt.Errorf("%w: --wait-timeout requires --wait-idle or --wait-for", errUsage)
	}
	if (waitForProvided || cfg.WaitFor != "") && cfg.WaitFor != "all" && cfg.WaitFor != "critical" {
		return fmt.Errorf("%w: --wait-for must be all or critical", errUsage)
	}
	cfg.Timeout = *timeout
	cfg.BinaryVersion = version
	cfg.UserValues = []string(userValues)
	cfg.UserSet = []string(userSet)
	cfg.BinarySchemaVersion = store.LatestMigrationVersion()

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

	result, err := installer.RunUpgrade(ctx, cfg, installer.Deps{
		Output:           output,
		RunHelm:          runHelm,
		RunCommand:       installer.DefaultRunCommand,
		RunCommandStream: installer.DefaultRunCommandStream,
		KubeClient:       kubeClient,
		RestConfig:       restConfig,
		ContextName:      selectedContext,
	})
	if err != nil {
		return err
	}
	return upgradeCompletionError(cfg.DryRun, result)
}

func upgradeCompletionError(dryRun bool, result installer.UpgradeResult) error {
	if dryRun || result.Upgraded {
		return nil
	}
	if result.AlreadyUpToDate {
		return errors.New("upgrade not performed: target version is already installed (no values/profile change requested)")
	}
	return errors.New("upgrade not performed")
}
