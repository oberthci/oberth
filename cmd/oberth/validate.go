package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func runValidate(_ context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var imagePrefixes string
	var containerDiskPrefixes string
	kubeVirt := flags.Bool("kubevirt", false, "validate KubeVirt VM templates and KVM device leaves, including guest-test leaves (server grant still required)")
	flags.StringVar(&containerDiskPrefixes, "container-disk-prefixes", "", "comma-separated containerDisk image prefixes for KubeVirt validation")
	maxVMs := flags.Int("kubevirt-max-vms", 0, "per-run VMI budget to validate against, as a KubeVirt grant would set it (default 2)")
	maxCores := flags.Int("kubevirt-max-cores", 0, "per-run peak VMI cores to validate against (default 6)")
	maxMemory := flags.String("kubevirt-max-memory", "", "per-run peak VMI memory to validate against (default 12Gi)")
	flags.Bool("allow-unresolved-fragments", false,
		"deprecated: unresolved fragments still fail validation because admission requires their pinned contents")
	flags.StringVar(&imagePrefixes, "runner-image-prefixes", strings.Join(pipeline.DefaultRunnerImagePrefixes, ","), "comma-separated allowlist of permitted runner image prefixes")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("%w: validate accepts at most one path (a repository root or a .oberth directory)", errUsage)
	}
	target, err := resolveValidateTarget(flags.Arg(0))
	if err != nil {
		return err
	}
	target.imagePrefixes = splitRunnerImagePrefixes(imagePrefixes)
	target.kubeVirt = *kubeVirt
	target.containerDiskPrefixes = splitRunnerImagePrefixes(containerDiskPrefixes)
	if *maxVMs != 0 || *maxCores != 0 || *maxMemory != "" {
		budget := store.KubeVirtBudget{MaxVMs: *maxVMs, MaxCores: *maxCores}
		if *maxMemory != "" {
			if budget.MaxMemoryBytes, err = store.ParseKubeVirtMemory(*maxMemory); err != nil {
				return fmt.Errorf("%w: %w", errUsage, err)
			}
		}
		budget = budget.WithDefaults()
		if err := budget.Validate(); err != nil {
			return fmt.Errorf("%w: %w", errUsage, err)
		}
		target.kubeVirtBudget = argoworkflow.VMIBudget{MaxVMs: budget.MaxVMs, MaxCores: budget.MaxCores, MaxMemory: *resource.NewQuantity(budget.MaxMemoryBytes, resource.BinarySI)}
	}
	return executeValidate(target, output)
}

// validateTarget names the locations every check derives from.
type validateTarget struct {
	repoRoot              string   // the repository root
	imagePrefixes         []string // administrator-allowed runner image prefixes
	kubeVirt              bool
	containerDiskPrefixes []string
	kubeVirtBudget        argoworkflow.VMIBudget // zero: the default grant budget
}

// resolveValidateTarget accepts the same spellings a developer reaches for:
// no argument (the working directory), a repository root, or a .oberth
// directory.
func resolveValidateTarget(argument string) (validateTarget, error) {
	if argument == "" {
		argument = "."
	}
	absolute, err := filepath.Abs(argument)
	if err != nil {
		return validateTarget{}, fmt.Errorf("resolve validate target %q: %w", argument, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return validateTarget{}, fmt.Errorf("validate target: %w", err)
	}
	if !info.IsDir() {
		return validateTarget{}, fmt.Errorf("validate target %q must be a directory", absolute)
	}
	// If the argument is a .oberth directory, use its parent.
	if filepath.Base(absolute) == ".oberth" {
		return validateTarget{repoRoot: filepath.Dir(absolute)}, nil
	}
	return validateTarget{repoRoot: absolute}, nil
}

type validateReport struct {
	out      io.Writer
	writeErr error
	errors   int
	skips    int
}

func (report *validateReport) line(format string, args ...any) {
	if report.writeErr != nil {
		return
	}
	_, report.writeErr = fmt.Fprintf(report.out, format+"\n", args...)
}

func (report *validateReport) problem(format string, args ...any) {
	report.errors++
	report.line("  error: "+format, args...)
}

func executeValidate(target validateTarget, output io.Writer) error {
	report := &validateReport{out: output}
	report.line("validate: %s", target.repoRoot)
	report.line("")

	for _, entry := range []struct {
		file    string
		trigger pipeline.Trigger
	}{
		{".oberth/build.yaml", pipeline.TriggerCI},
		{".oberth/release.yaml", pipeline.TriggerRelease},
	} {
		raw, err := readConfinedPipelineFile(target.repoRoot, entry.file)
		if err != nil {
			if os.IsNotExist(err) {
				if entry.trigger == pipeline.TriggerCI {
					report.problem("%s not found: every repository needs a CI pipeline", entry.file)
				} else {
					report.line("  %s  not present (optional)", entry.file)
				}
				continue
			}
			report.problem("read %s: %v", entry.file, err)
			continue
		}
		report.line("%s (%d bytes)", entry.file, len(raw))

		workflow, err := argoworkflow.Decode(raw)
		if err != nil {
			report.problem("decode %s: %v", entry.file, err)
			continue
		}
		report.line("  ok  YAML decode (strict)")

		references, refErr := argoworkflow.FragmentRefs(workflow)
		if refErr != nil {
			report.problem("fragment references in %s: %v", entry.file, refErr)
			continue
		}
		if len(references) != 0 {
			// No offline fragment loader is configured here. Runtime admission
			// runs after Resolve expands the pinned fragment contents; without
			// them none of this document's admission checks can be certified.
			for _, reference := range references {
				report.problem("fragment %s cannot be resolved offline; admission requires the fully resolved pipeline with pinned fragment contents. "+
					"The server resolves and admits it at push time; --allow-unresolved-fragments cannot bypass admission", reference)
			}
			continue
		}

		if err := argoworkflow.Admit(workflow, argoworkflow.Policy{RunnerImagePrefixes: target.imagePrefixes, KubeVirt: target.kubeVirt, ContainerDiskPrefixes: target.containerDiskPrefixes, VMIBudget: target.kubeVirtBudget}); err != nil {
			report.problem("admission %s: %v", entry.file, err)
			continue
		}
		report.line("  ok  admission (%s trigger)", entry.trigger)

		if err := argoworkflow.AdmitSizeDeclarations(workflow, entry.trigger); err != nil {
			report.problem("size declaration %s: %v", entry.file, err)
			continue
		}
		report.line("  ok  size %s", describeSizes(workflow, entry.trigger))

		// The step inventory the server will seed for a run of this document,
		// printed from the same enumeration, so what a push shows is knowable
		// before the push.
		planned, err := argoworkflow.PlannedSteps(workflow)
		if err != nil {
			report.problem("enumerate steps in %s: %v", entry.file, err)
			continue
		}
		stages := 0
		previous := ""
		for _, step := range planned {
			if step.Stage != previous {
				stages++
				previous = step.Stage
			}
		}
		report.line("  ok  %d step(s) across %d burn(s)", len(planned), stages)
		for _, step := range planned {
			report.line("      %2d  %s / %s", step.Ordinal, step.Stage, step.Step)
		}
	}

	report.line("")
	switch {
	case report.errors > 0:
		report.line("result: FAIL (%d error(s))", report.errors)
	case report.skips > 0:
		report.line("result: PASS (%d check(s) skipped)", report.skips)
	default:
		report.line("result: PASS")
	}
	if report.writeErr != nil {
		return report.writeErr
	}
	if report.errors > 0 {
		return fmt.Errorf("validation failed: %d error(s)", report.errors)
	}
	return nil
}

// readConfinedPipelineFile reads a pipeline file from within the repository
// root without following symlinks. Lstat rejects symlinks (both internal and
// external) before any read; os.OpenRoot confines subsequent path resolution
// to the root directory as defense in depth.
func readConfinedPipelineFile(repoRoot, relative string) ([]byte, error) {
	fullPath := filepath.Join(repoRoot, relative)
	info, err := os.Lstat(fullPath)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink", relative)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", relative)
	}
	if info.Size() > pipeline.MaxSourceBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", relative, pipeline.MaxSourceBytes)
	}
	root, err := os.OpenRoot(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("open repository root: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	source, err := io.ReadAll(io.LimitReader(file, pipeline.MaxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", relative, err)
	}
	if int64(len(source)) > pipeline.MaxSourceBytes {
		return nil, fmt.Errorf("%s exceeds the source-size limit", relative)
	}
	return source, nil
}

// describeSizes prints the admission weight each run trigger of this document
// resolves to, so what a push, a promotion and a schedule will each reserve is
// knowable before the push.
func describeSizes(workflow *wfv1.Workflow, tier pipeline.Trigger) string {
	var parts []string
	for _, trigger := range argoworkflow.RunTriggers() {
		if trigger.Tier() != tier {
			continue
		}
		size, err := argoworkflow.DeclaredSizeFor(workflow, trigger)
		if err != nil {
			return err.Error()
		}
		parts = append(parts, string(trigger)+"="+string(size))
	}
	return strings.Join(parts, " ")
}
