package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/runner"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const (
	maxPipelineSourceBytes = 1 << 20
	maxSecretFileBytes     = 1 << 20
	maxSecretTotalBytes    = 8 << 20
	maxSecretFiles         = 1024
)

type options struct {
	sourceRoot       string
	periapsisPath    string
	trigger          periapsis.Trigger
	stepTimeout      time.Duration
	terminationPath  string
	secretRoot       string
	secretStoreRoot  string
	receiveStoreRoot string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) (exitCode int) {
	started := time.Now().UTC()
	masker := runner.NewMasker(nil)
	trigger := periapsis.TriggerCI
	var termination *os.File
	defer func() {
		if recovered := recover(); recovered != nil {
			err := fmt.Errorf("runner panic: %v", recovered)
			writeFailureSummary(termination, trigger, started, masker, err)
			writeRunnerMessage(stderr, masker, err.Error())
			exitCode = 1
		}
		if termination != nil {
			_ = termination.Close()
		}
	}()

	configuration, err := parseOptions(args, getenv)
	if err != nil {
		termination, _ = os.OpenFile(configuration.terminationPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		writeFailureSummary(termination, trigger, started, masker, err)
		writeRunnerMessage(stderr, masker, err.Error())
		return 1
	}
	if configuration.receiveStoreRoot != "" {
		// Exec-helper mode: the Oberth server pipes the secret store payload on
		// stdin; this invocation only materializes it on the memory-backed
		// volume and must never touch the main runner's termination log.
		if err := runner.ReceiveSecretStoreSecrets(stdin, configuration.receiveStoreRoot); err != nil {
			writeRunnerMessage(stderr, masker, fmt.Sprintf("receive secret store secrets: %v", err))
			return 1
		}
		return 0
	}
	trigger = configuration.trigger
	termination, err = os.OpenFile(configuration.terminationPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		writeRunnerMessage(stderr, masker, fmt.Sprintf("open termination log: %v", err))
		return 1
	}

	secrets, err := loadSecrets(configuration.secretRoot)
	if err != nil {
		writeFailureSummary(termination, trigger, started, masker, err)
		writeRunnerMessage(stderr, masker, err.Error())
		return 1
	}
	if configuration.secretStoreRoot != "" {
		// Store-sourced secrets arrive after the container starts; burns must
		// not begin until the delivery verifies, and every delivered value
		// joins the masker before any step can echo it.
		storeValues, err := runner.AwaitSecretStoreSecrets(ctx, configuration.secretStoreRoot, runner.DefaultSecretStoreAwaitTimeout)
		if err != nil {
			writeFailureSummary(termination, trigger, started, masker, err)
			writeRunnerMessage(stderr, masker, err.Error())
			return 1
		}
		secrets = append(secrets, storeValues...)
	}
	masker = runner.NewMasker(secrets)
	source, resolvedRoot, err := readPipelineSource(configuration.sourceRoot, configuration.periapsisPath)
	if err != nil {
		writeFailureSummary(termination, trigger, started, masker, err)
		writeRunnerMessage(stderr, masker, err.Error())
		return 1
	}
	tag := ""
	if trigger == periapsis.TriggerRelease {
		tag = getenv("OBERTH_REF")
	}
	pipelineContext := periapsis.NewContext(getenv("OBERTH_REPO"), getenv("OBERTH_REF"), getenv("OBERTH_SHA"), tag)
	pipeline, err := periapsis.Interpret(string(source), pipelineContext)
	if err != nil {
		writeFailureSummary(termination, trigger, started, masker, err)
		writeRunnerMessage(stderr, masker, err.Error())
		return 1
	}

	execution := runner.Runner{
		Trigger:       trigger,
		StepTimeout:   configuration.stepTimeout,
		Secrets:       secrets,
		SummaryWriter: termination,
	}
	if _, err := execution.Run(ctx, *pipeline, resolvedRoot, stdout); err != nil {
		writeRunnerMessage(stderr, masker, err.Error())
		return 1
	}
	return 0
}

func parseOptions(args []string, getenv func(string) string) (options, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	triggerValue := getenv("OBERTH_TRIGGER")
	if triggerValue == "" {
		triggerValue = string(periapsis.TriggerCI)
	}
	configuration := options{}
	flags := flag.NewFlagSet("oberth-runner", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&configuration.sourceRoot, "src", ".", "checked-out source root")
	flags.StringVar(&configuration.periapsisPath, "periapsis", ".oberth/periapsis.go", "pipeline path relative to source root")
	flags.StringVar(&triggerValue, "trigger", triggerValue, "ci or release")
	flags.DurationVar(&configuration.stepTimeout, "step-timeout", runner.DefaultStepTimeout, "default timeout for each step")
	flags.StringVar(&configuration.terminationPath, "termination-log", "/dev/termination-log", "termination summary path")
	flags.StringVar(&configuration.secretRoot, "secret-dir", "", "mounted secrets root")
	flags.StringVar(&configuration.secretStoreRoot, "secretstore-dir", "", "await exec-delivered secret store entries under this memory-backed directory")
	flags.StringVar(&configuration.receiveStoreRoot, "receive-secretstore", "", "helper mode: read one secret store payload from stdin into this directory and exit")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return configuration, errors.New("invalid runner arguments")
	}
	if configuration.stepTimeout <= 0 {
		return configuration, errors.New("step timeout must be greater than zero")
	}
	if strings.TrimSpace(configuration.terminationPath) == "" || strings.ContainsRune(configuration.terminationPath, '\x00') {
		return configuration, errors.New("termination log path is required")
	}
	trigger, err := periapsis.ParseTrigger(triggerValue)
	if err != nil {
		return configuration, err
	}
	configuration.trigger = trigger
	return configuration, nil
}

func readPipelineSource(sourceRoot, relativePath string) ([]byte, string, error) {
	if relativePath == "" || !filepath.IsLocal(relativePath) || filepath.Clean(relativePath) != relativePath || relativePath == "." || strings.ContainsRune(relativePath, '\x00') {
		return nil, "", errors.New("periapsis path must be a clean local file path")
	}
	absoluteRoot, err := filepath.Abs(sourceRoot)
	if err != nil {
		return nil, "", fmt.Errorf("resolve source root: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return nil, "", fmt.Errorf("resolve source root: %w", err)
	}
	rootInfo, err := os.Stat(resolvedRoot)
	if err != nil {
		return nil, "", fmt.Errorf("inspect source root: %w", err)
	}
	if !rootInfo.IsDir() {
		return nil, "", errors.New("source root is not a directory")
	}
	resolvedPath, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, relativePath))
	if err != nil {
		return nil, "", fmt.Errorf("resolve periapsis source: %w", err)
	}
	if !pathWithin(resolvedRoot, resolvedPath) {
		return nil, "", errors.New("periapsis source resolves outside source root")
	}
	raw, err := readBoundedRegularFile(resolvedPath, maxPipelineSourceBytes)
	if err != nil {
		return nil, "", fmt.Errorf("read periapsis source: %w", err)
	}
	return raw, resolvedRoot, nil
}

func loadSecrets(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve secrets root: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve secrets root: %w", err)
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil {
		return nil, fmt.Errorf("inspect secrets root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("secrets root is not a directory")
	}
	seen := make(map[string]struct{})
	values := make([]string, 0)
	total := 0
	err = filepath.WalkDir(resolvedRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() && entry.Type()&os.ModeSymlink == 0 {
			return fmt.Errorf("secret path %q is not a regular file", path)
		}
		resolvedPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		if !pathWithin(resolvedRoot, resolvedPath) {
			return fmt.Errorf("secret path %q resolves outside secrets root", path)
		}
		resolvedInfo, err := os.Stat(resolvedPath)
		if err != nil {
			return err
		}
		if resolvedInfo.IsDir() {
			return nil
		}
		if !resolvedInfo.Mode().IsRegular() {
			return fmt.Errorf("secret path %q is not a regular file", path)
		}
		if _, duplicate := seen[resolvedPath]; duplicate {
			return nil
		}
		seen[resolvedPath] = struct{}{}
		if len(seen) > maxSecretFiles {
			return fmt.Errorf("secrets root exceeds %d files", maxSecretFiles)
		}
		raw, err := readBoundedRegularFile(resolvedPath, maxSecretFileBytes)
		if err != nil {
			return err
		}
		total += len(raw)
		if total > maxSecretTotalBytes {
			return fmt.Errorf("secrets root exceeds %d bytes", maxSecretTotalBytes)
		}
		if len(raw) > 0 {
			values = append(values, string(raw))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load secrets: %w", err)
	}
	return values, nil
}

func readBoundedRegularFile(path string, maximum int64) ([]byte, error) {
	// #nosec G304 -- callers resolve the path and verify it stays within the trusted root.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	if info.Size() > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	return raw, nil
}

func pathWithin(root, path string) bool { return gitoid.PathWithin(root, path) }

func writeFailureSummary(writer io.Writer, trigger periapsis.Trigger, started time.Time, masker runner.Masker, failure error) {
	if writer == nil {
		return
	}
	startedAt := started.Format(time.RFC3339Nano)
	finishedAt := time.Now().UTC().Format(time.RFC3339Nano)
	summary := runner.Summary{
		Version:    runner.SummaryVersion,
		Trigger:    string(trigger),
		Status:     runner.StatusFailed,
		Error:      masker.Mask(failure.Error()),
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		Steps: []runner.StepResult{{
			Burn: "runner", Step: "runner", Status: runner.StepFailed, ExitCode: 1,
			StartedAt: startedAt, FinishedAt: finishedAt,
		}},
	}
	_ = runner.WriteSummary(writer, summary)
}

func writeRunnerMessage(writer io.Writer, masker runner.Masker, message string) {
	if writer == nil {
		return
	}
	message = strings.ReplaceAll(masker.Mask(message), "\r\n", "\n")
	lines := strings.Split(message, "\n")
	for index, line := range lines {
		if index == len(lines)-1 && line == "" {
			continue
		}
		_, _ = fmt.Fprintf(writer, "[runner/runner] %s\n", strings.TrimSuffix(line, "\r"))
	}
}
