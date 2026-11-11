package argoworkflow

import (
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// Repository annotations are read statically so the reviewed source and
// admission decision cannot diverge through evaluated repository code.
const (
	// SecretPathsAnnotation declares the OpenBao paths credentialed steps
	// intend to read through secretstore exec. Admission checks the approval
	// table and chooses the trigger-appropriate ServiceAccount. OpenBao role
	// policies remain the authority for actual reads; declarations record intent.
	SecretPathsAnnotation = "oberth.ci/secret-paths"
)

const maxDeclaredSecretPaths = 32

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

// removedAnnotations are pipeline annotations Oberth no longer reads. A
// document that carries one is refused, naming what replaced it, instead of
// running as if it took effect.
var removedAnnotations = map[string]string{
	"oberth.ci/size":              "admission counts runs and Kubernetes places each Pod by its resources block; delete the annotation",
	"oberth.ci/size-by-trigger":   "admission counts runs and Kubernetes places each Pod by its resources block; delete the annotation",
	"oberth.ci/concurrency-group": "declare a native Argo mutex or semaphore (spec.synchronization) instead",
	"oberth.ci/concurrency-limit": "declare a native Argo mutex or semaphore (spec.synchronization) instead",
	"oberth.ci/exclusive":         "declare a native Argo semaphore (template synchronization, configMapKeyRef oberth-sync-limits) instead",
}

func removedAnnotation(location, name string) error {
	return fmt.Errorf("argoworkflow: %s annotation %s is removed: %s", location, name, removedAnnotations[name])
}
