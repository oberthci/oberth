package argoworkflow

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// Repository annotations are read statically so the reviewed source and
// admission decision cannot diverge through evaluated repository code.
const (
	// SizeAnnotation names the pipeline's historical S/M/L/XL tier. It is
	// recorded with the run and shown, nothing more: admission counts runs
	// and Kubernetes places each Pod by its own resources block.
	SizeAnnotation = "oberth.ci/size"

	// RetiredExclusiveAnnotation is the former per-step exclusive-class
	// declaration. Since v0.18.0 it is accepted and ignored, so documents
	// that still carry it keep running; steps that need a shared limit
	// declare native Argo synchronization instead.
	RetiredExclusiveAnnotation = "oberth.ci/exclusive"

	// ConcurrencyGroupAnnotation names a group shared across repositories and
	// all triggers. ConcurrencyLimitAnnotation defaults to one; YAML declarations
	// of the same group must agree. Names use 1..63 lowercase letters/digits/hyphens.
	ConcurrencyGroupAnnotation = "oberth.ci/concurrency-group"
	ConcurrencyLimitAnnotation = "oberth.ci/concurrency-limit"

	// SecretPathsAnnotation declares the OpenBao paths credentialed steps
	// intend to read through secretstore exec. Admission checks the approval
	// table and chooses the trigger-appropriate ServiceAccount. OpenBao role
	// policies remain the authority for actual reads; declarations record intent.
	SecretPathsAnnotation = "oberth.ci/secret-paths"
)

const maxDeclaredSecretPaths = 32

// MaxConcurrencyGroupLength bounds a concurrency group name.
const MaxConcurrencyGroupLength = 63

// ValidConcurrencyGroup reports whether name is a legal concurrency group:
// 1 to 63 characters, each a lowercase ASCII letter, digit, or hyphen. The
// store's schema enforces the same rule, so a name that passes here is the
// only kind that can ever be recorded on a run.
func ValidConcurrencyGroup(name string) bool {
	if name == "" || len(name) > MaxConcurrencyGroupLength {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

// DeclaredConcurrencyGroup returns the concurrency group the document declares,
// or "" when it declares none. Surrounding whitespace is ignored exactly as it
// is for the size annotation; the name itself is never normalized, so a value
// that is not already a legal group name is an admission error rather than a
// silently different group.
func DeclaredConcurrencyGroup(workflow *wfv1.Workflow) (string, error) {
	if workflow == nil {
		return "", errors.New("argoworkflow: no Workflow to read")
	}
	declared, ok := workflow.Annotations[ConcurrencyGroupAnnotation]
	if !ok || strings.TrimSpace(declared) == "" {
		return "", nil
	}
	group := strings.TrimSpace(declared)
	if !ValidConcurrencyGroup(group) {
		return "", fmt.Errorf("argoworkflow: annotation %s declares invalid concurrency group %q; "+
			"a group is 1 to %d characters of lowercase letters, digits, and hyphens ([a-z0-9-])",
			ConcurrencyGroupAnnotation, declared, MaxConcurrencyGroupLength)
	}
	return group, nil
}

// DeclaredConcurrencyLimit reads a positive decimal limit from immutable YAML.
// Omission defaults to one. A limit without a group is a configuration error.
func DeclaredConcurrencyLimit(workflow *wfv1.Workflow) (int, error) {
	group, err := DeclaredConcurrencyGroup(workflow)
	if err != nil {
		return 0, err
	}
	raw, present := workflow.Annotations[ConcurrencyLimitAnnotation]
	if !present {
		return 1, nil
	}
	value := strings.TrimSpace(raw)
	if group == "" || value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, fmt.Errorf("argoworkflow: annotation %s requires a concurrency group and a positive decimal integer", ConcurrencyLimitAnnotation)
	}
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("argoworkflow: annotation %s must be in 1..2147483647", ConcurrencyLimitAnnotation)
	}
	return int(n), nil
}

// DeclaredSize returns the scheduler admission weight the document requests.
func DeclaredSize(workflow *wfv1.Workflow) (pipeline.Size, error) {
	if workflow == nil {
		return "", errors.New("argoworkflow: no Workflow to read")
	}
	declared, ok := workflow.Annotations[SizeAnnotation]
	if !ok || strings.TrimSpace(declared) == "" {
		return pipeline.M, nil
	}
	size := pipeline.Size(strings.TrimSpace(declared))
	switch size {
	case pipeline.S, pipeline.M, pipeline.L, pipeline.XL:
		return size, nil
	default:
		return "", fmt.Errorf("argoworkflow: annotation %s declares unknown size %q; sizes are %q, %q, %q, and %q",
			SizeAnnotation, declared, pipeline.S, pipeline.M, pipeline.L, pipeline.XL)
	}
}

// DeclaredSecretPaths returns the OpenBao KV paths the document declares it
// will read in-Pod, in declaration order with duplicates rejected.
func DeclaredSecretPaths(workflow *wfv1.Workflow) ([]string, error) {
	if workflow == nil {
		return nil, errors.New("argoworkflow: no Workflow to read")
	}
	declared, ok := workflow.Annotations[SecretPathsAnnotation]
	if !ok || strings.TrimSpace(declared) == "" {
		return nil, nil
	}
	fields := strings.Split(declared, ",")
	if len(fields) > maxDeclaredSecretPaths {
		return nil, fmt.Errorf("argoworkflow: annotation %s declares %d paths, maximum is %d",
			SecretPathsAnnotation, len(fields), maxDeclaredSecretPaths)
	}
	paths := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for index, field := range fields {
		path := strings.TrimSpace(field)
		if err := pipeline.ValidateSecretStorePath(path); err != nil {
			return nil, fmt.Errorf("argoworkflow: annotation %s entry %d: %w", SecretPathsAnnotation, index, err)
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, fmt.Errorf("argoworkflow: annotation %s declares %q more than once", SecretPathsAnnotation, path)
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths, nil
}

// UsesEnvconsul reports whether a template's main container command is the
// envconsul binary, the legacy credential chain that needs the secret store
// login token.
func UsesEnvconsul(template *wfv1.Template) bool {
	isEnvconsul := func(command []string) bool {
		command = resourceUsageCommand(command)
		return len(command) > 0 &&
			(strings.HasSuffix(command[0], "/envconsul") || command[0] == "envconsul")
	}
	if template.Container != nil && isEnvconsul(template.Container.Command) {
		return true
	}
	if template.Script != nil && isEnvconsul(template.Script.Command) {
		return true
	}
	return false
}

// UsesOberthSecretstore reports whether a template's main container command
// is `oberth secretstore exec`, the native
// credential chain that handles the store fetch, file materialisation, env
// stripping, and redaction in a single binary.
func UsesOberthSecretstore(template *wfv1.Template) bool {
	isOberthSecretstore := func(command, args []string) bool {
		command = resourceUsageCommand(command)
		if len(command) == 0 {
			return false
		}
		// command[0] is the binary path; args carries the subcommands and flags.
		if !strings.HasSuffix(command[0], "/oberth") && command[0] != "oberth" {
			return false
		}
		// The subcommand words may be in command[1:] or in args[0:].
		rest := append(command[1:], args...)
		if len(rest) < 2 {
			return false
		}
		return rest[0] == "secretstore" && rest[1] == "exec"
	}
	if template.Container != nil &&
		isOberthSecretstore(template.Container.Command, template.Container.Args) {
		return true
	}
	if template.Script != nil &&
		isOberthSecretstore(template.Script.Command, template.Script.Args) {
		return true
	}
	return false
}

// UsesSecrets reports whether a template runs any recognised credential
// chain: envconsul (legacy) or oberth secretstore exec (native).
// The server mounts the secret store login token into exactly these Pods and
// no others, so this is also what marks a planned step as one that reads
// secrets.
func UsesSecrets(template *wfv1.Template) bool {
	return UsesEnvconsul(template) || UsesOberthSecretstore(template)
}

// UsesCredentials is the shared credential-admission and replay-plan predicate.
// WIF grants an audience-scoped token even without a secret-store command.
func UsesCredentials(template *wfv1.Template) bool {
	if template == nil {
		return false
	}
	return UsesSecrets(template) || UsesReleaseWIF(template)
}

// UsesReleaseWIF is also used by the capability admission walk.
func UsesReleaseWIF(template *wfv1.Template) bool {
	if template == nil {
		return false
	}
	_, present := template.Metadata.Annotations[ReleaseWIFRoleAnnotation]
	return present
}

// Resource measurement is outside the credential chain in admitted specs.
// Keep static plans and post-admission credential classification identical.
func resourceUsageCommand(command []string) []string {
	if len(command) > 3 && command[0] == "/run/oberth/usage/oberth" && command[1] == "internal-step-exec" && command[2] == "--" {
		return command[3:]
	}
	return command
}
