package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/oberthci/oberth/internal/installer"
)

func runPreflight(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: preflight drift", errUsage)
	}
	switch arguments[0] {
	case "drift":
		return runPreflightDrift(ctx, arguments[1:], output, installer.DefaultRunHelm)
	default:
		return fmt.Errorf("%w: unknown preflight command %q", errUsage, arguments[0])
	}
}

// helmRunner matches the installer.Deps.RunHelm signature.
type helmRunner func(ctx context.Context, args []string) ([]byte, error)

// runPreflightDrift renders the local chart against the live release's merged
// values and exits non-zero when the template fails. This catches the
// --reuse-values class of drift where a new chart value is absent from a live
// release: helm template renders with the live values only, and any required
// value that the live release never saw surfaces as a template error before
// the tag is pushed.
//
// The previous shell-pipeline version had a confirmed silent-no-op failure
// mode (#452): `helm get values` errored to stderr, the pipe delivered empty
// stdin, and `helm template` rendered chart defaults and exited 0 — verifying
// nothing. This command closes that gap by refusing to proceed when the
// values document is empty or the release is not found.
func runPreflightDrift(ctx context.Context, arguments []string, output io.Writer, runHelm helmRunner) error {
	flags := flag.NewFlagSet("preflight drift", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	chart := flags.String("chart", "charts/oberth", "path to the local chart directory")
	release := flags.String("release", "oberth", "helm release name")
	namespace := flags.String("namespace", "oberth", "helm release namespace")
	kubeContext := flags.String("context", "", "kubeconfig context (default: current context)")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: preflight drift accepts flags only", errUsage)
	}

	if runHelm == nil {
		runHelm = installer.DefaultRunHelm
	}

	// Step 1: Retrieve the live release's merged values.
	getValuesArgs := []string{"get", "values", *release, "-n", *namespace, "-o", "yaml"}
	if *kubeContext != "" {
		getValuesArgs = append(getValuesArgs, "--kube-context", *kubeContext)
	}
	valuesOut, err := runHelm(ctx, getValuesArgs)
	if err != nil {
		return fmt.Errorf("helm get values failed (release %q in namespace %q may not exist): %w", *release, *namespace, err)
	}

	// Step 2: Refuse empty or whitespace-only values. This is the exact
	// failure mode that #452 documented: an error on stderr with empty stdout
	// causes `helm template` to render chart defaults and exit 0.
	trimmed := strings.TrimSpace(string(valuesOut))
	if trimmed == "" {
		return fmt.Errorf("live values document is empty for release %q in namespace %q; refusing to template against chart defaults", *release, *namespace)
	}
	// helm get values returns "null" when the release has no user-supplied
	// values (only chart defaults). That is a valid state — but templating
	// against literal "null" YAML is equivalent to templating with zero
	// overrides, which is the same as chart defaults. Warn but do not block:
	// the operator may genuinely have a release with no overrides.
	if trimmed == "null" {
		_, _ = fmt.Fprintln(output, "WARNING: helm get values returned \"null\" (no user-supplied values); templating against chart defaults only")
	}

	// Step 3: Write values to a temp file for helm template.
	valuesFile, err := os.CreateTemp("", "oberth-preflight-drift-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary values file: %w", err)
	}
	valuesPath := valuesFile.Name()
	defer func() { _ = os.Remove(valuesPath) }()
	if _, err := valuesFile.Write(valuesOut); err != nil {
		_ = valuesFile.Close()
		return fmt.Errorf("write temporary values file: %w", err)
	}
	if err := valuesFile.Close(); err != nil {
		return fmt.Errorf("close temporary values file: %w", err)
	}

	// Step 4: Template the local chart against the live values.
	templateArgs := []string{"template", *release, *chart, "-n", *namespace, "-f", valuesPath}
	if *kubeContext != "" {
		templateArgs = append(templateArgs, "--kube-context", *kubeContext)
	}
	_, templateErr := runHelm(ctx, templateArgs)
	if templateErr != nil {
		return fmt.Errorf("chart template failed against live values — a release upgrade would fail the same way: %w", templateErr)
	}

	_, _ = fmt.Fprintf(output, "preflight drift: OK — chart %s templates cleanly against live values from release %s/%s\n", *chart, *namespace, *release)
	return nil
}
