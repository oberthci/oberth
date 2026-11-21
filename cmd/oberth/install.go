package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
)

// installationDispatch keeps command tests independent of cluster access.
type installationRequest struct {
	name  string
	flags *flag.FlagSet
	args  []string
}
type installationDispatch func(context.Context, installationRequest, io.Reader, io.Writer) error

func (a application) installEntry(ctx context.Context, args []string) (int, bool) {
	for _, row := range installationCommands() {
		words := strings.Fields(row.name)
		if len(args) < len(words) || strings.Join(args[:len(words)], " ") != row.name {
			continue
		}
		return a.runInstallation(ctx, row, args), true
	}
	return 0, false
}

func (a application) runInstallation(ctx context.Context, row installationCommand, args []string) int {
	words := strings.Fields(row.name)
	flags := installationFlags(row.name)
	rest, err := installationArgs(row, flags, args[len(words):])
	if err != nil {
		var refusal *publicerr.Error
		if errors.As(err, &refusal) {
			a.failed(refusal)
			return 2
		}
		return a.usage(append(installationProblems(err), installationUsage(row.name)))
	}
	if len(rest) < row.minArgs || len(rest) > row.maxArgs {
		return a.usage([]string{installationUsage(row.name)})
	}
	request := installationRequest{row.name, flags, rest}
	if err = installationOutput(request); err != nil {
		return a.usage(append(installationProblems(err), installationUsage(row.name)))
	}
	run := a.installationRunner(row.name)
	if err = run(ctx, request, a.in, a.out); err != nil {
		if code, ok := stepExitCode(err); row.name == "secretstore exec" && ok {
			return code
		}
		return a.installationFailed(err, request)
	}
	return 0
}

func (a application) installationFailed(err error, request installationRequest) int {
	if request.name != "install" || !request.enabled("verify") || request.enabled("detail") {
		return a.failedDetail(installationFailure(err), request.enabled("detail"))
	}
	refusal := errorRefusal(installationFailure(err))
	facts := append([]string{refusal.Reason, refusal.Remedy}, refusal.Context...)
	_, _ = fmt.Fprintln(a.diagnostic, strings.ReplaceAll(safeDisplay(strings.Join(facts, " ")), "\n", " "))
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}

func installationFailure(err error) error {
	if errors.Is(err, context.Canceled) {
		return publicerr.New(publicerr.Unavailable, "Installation interrupted.", "Run oberth system status before retrying.", err)
	}
	var refusal *publicerr.Error
	if errors.As(err, &refusal) && (refusal.ID == releaseRedirectID || refusal.ID == localReleaseFilesID) {
		return refusal
	}
	var problem *installer.Problem
	if !errors.As(err, &problem) {
		return err
	}
	public := publicerr.New(publicerr.InvalidInput, problem.Reason, problem.Remedy, err)
	if problem.Detail != "" {
		public.Context = []string{problem.Detail}
	}
	return public
}

func stepExitCode(err error) (int, bool) {
	if errors.Is(err, context.Canceled) {
		return 0, false
	}
	var public interface {
		Reason() string
		Remedy() string
	}
	if errors.As(err, &public) {
		return 0, false
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return exit.ExitCode(), true
	}
	return 0, false
}

func installationArgs(row installationCommand, flags *flag.FlagSet, args []string) ([]string, error) {
	if row.name == "upgrade" {
		for _, arg := range args {
			if arg == "--chart" || strings.HasPrefix(arg, "--chart=") {
				return nil, publicerr.New(publicerr.InvalidInput, "Upgrades take signed releases.", "Use oberth install --chart <directory> for a local chart.", nil)
			}
		}
	}

	if row.name == "secretstore exec" {
		separator := slices.Index(args, "--")
		if separator < 0 || separator == len(args)-1 {
			return nil, usageProblems{"Use oberth secretstore exec -- <command> [args]."}
		}
		if err := flags.Parse(args[:separator]); err != nil {
			return nil, err
		}
		if flags.NArg() != 0 {
			return nil, usageProblems{"Place step options before -- and the command after it."}
		}
		return args[separator+1:], nil
	}
	if row.name == "secret put" {
		if err := selectorOrder(flags, args, false); err != nil {
			return nil, err
		}
	}
	return parseFlow(flags, args)
}

func (a application) installationRunner(name string) installationDispatch {
	if name == "secretstore exec" {
		if a.secretExec != nil {
			return a.secretExec
		}
		return a.runSecretStep
	}
	if a.install != nil {
		return a.install
	}
	return a.runInstaller
}

func installationSynopsis(name string) string {
	if name == "secretstore exec" {
		return name + " -- <command> [args]"
	}
	switch name {
	case "secret put":
		return name + " <secret-path>"
	}
	return name
}
func installationUsage(name string) string {
	if name == "secretstore exec" {
		return "Use oberth " + installationSynopsis(name) + "."
	}
	return "Invalid installation arguments; run oberth " + name + " --help."
}

func installationProblems(err error) []string {
	var problems usageProblems
	if errors.As(err, &problems) {
		return problems
	}
	return []string{safeDisplay(err.Error())}
}
