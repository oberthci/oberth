package argoworkflow

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	yamlv3 "go.yaml.in/yaml/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"maps"
	"strings"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

type message struct{ reason, remedy string }

// Each refusal uses this catalog; field paths carry the individual offenders.
func messages() map[string]message {
	reason, remedy := runtimeconfig.SecretStoreConfiguration()
	reason = strings.TrimSuffix(reason, ".")
	catalogue := literalMessages()
	maps.Copy(catalogue, parameterMessages())
	maps.Copy(catalogue, nativeMessages())
	maps.Copy(catalogue, grantMessages())
	catalogue["store_config"] = message{reason, remedy}
	catalogue["store_unverified"] = message{"Secret store configuration is unverified", remedy}
	return catalogue
}

func parameterMessages() map[string]message {
	return map[string]message{
		"oberth-sha":         {"oberth-sha is only for schedule parents", "Read the admitted commit with git -C /work/src rev-parse HEAD; declare the main source input."},
		"oberth-ref":         {"oberth-ref is only for schedule parents", "For other runs, pass your own explicit branch-name input."},
		"oberth-repo":        {"oberth-repo is not supplied", "No repository-name parameter replaces it; pass your own explicit workflow input."},
		"oberth-release-tag": {"oberth-release-tag is not supplied", "Use workflow.parameters.version in a release workflow."},
		"oberth-release-sha": {"oberth-release-sha is not supplied", "Use workflow.parameters.release-tested-commit in a release workflow."},
	}
}

func nativeMessages() map[string]message {
	return map[string]message{
		"native_more": {"more native faults remain", "The 20 faults listed are a sample; fix them and validate again to see the rest."},
		"native_work": {"native validation exceeds its work limit", "Reduce independent validation scopes and references to at most 65,536 inspection operations."},
		"native":      {"native Workflow schema validation failed", "Correct the native template, input or invocation described in the detail."},
	}
}

// grantMessages are the refusals of a field that needs an administrator grant.
func grantMessages() map[string]message {
	return map[string]message{
		"shared_grant":      {"shared templates inherit no consumer grants", "Remove the selector or put this step in the repository file before granting it."},
		"read":              {"field reads unapproved cluster data", "Remove the field, or ask the operator to grant this cluster object to this step."},
		"grant":             {"credential declaration has no exact operator grant", "Ask the operator to approve this repository, file, step, path and exact audience."},
		"pvc":               {"PVC has no repository grant", "Use an allowed repository PVC, or ask the operator for its exact CI or release grant."},
		"device":            {"device resource has no step grant", "Remove the request, or ask the operator to grant this resource to this step."},
		"resource_name":     {"resource name exceeds 317 bytes", "Use a qualified resource name of at most 317 bytes."},
		"device_annotation": {"device annotations bypass device grants", "Remove the annotation; request the device under resources and ask for its device grant."},
	}
}

func literalMessages() map[string]message {
	return map[string]message{
		"file_size":             {"selected file path exceeds 1024 bytes", "Reduce the selected repository file path to at most 1024 bytes."},
		"tag_count":             {"source inventory exceeds 4096 retained tags", "Reduce the bound source inventory to at most 4096 retained tags."},
		"inspection":            {"template inspection exceeds its work limit", "Reduce template declarations and calls to at most 65,536 inspection operations."},
		"document_size":         {"workflow document exceeds 1 MiB", "Reduce the repository YAML document to at most 1 MiB."},
		"template_size":         {"shared template exceeds 1 MiB", "Reduce the named shared template spec to at most 1 MiB."},
		"defaults_size":         {"workflow defaults exceed 1 MiB", "Reduce the workflow defaults to at most 1 MiB."},
		"source_size":           {"source-consuming spec exceeds 512 KiB", "Reduce the submitted source-consuming spec to at most 512 KiB; metadata and status do not count."},
		"manifest_size":         {"resource manifest exceeds 128 KiB", "Reduce the inline resource manifest to at most 128 KiB."},
		"secret_paths":          {"step calls the secret helper but declares no path", "List the paths this step reads in its oberth.ci/secret-paths annotation."},
		"secret_path":           {"secret path is malformed or listed twice", "Use letters, digits, dot, underscore, hyphen and slash; no empty part, no .., no repeats."},
		"secret_form":           {"only a container, script or container-set step reads secrets", "Declare the paths on the container step that reads them."},
		"workflow_secret_paths": {"secret paths are declared for the whole workflow", "Move each path to the oberth.ci/secret-paths annotation of the steps that use it."},
		"release_resource":      {"resource templates do not run in a release", "Move this step to the build pipeline."},
		"secret_count":          {"more than 32 secret paths are declared", "Reduce the declared secret paths to at most 32."},
		"pin_count":             {"more than four source pins are declared", "Reduce unique pinned source destinations to at most four."},
		"kind_count":            {"more than 32 resource kinds are granted", "Reduce the resource capability to at most 32 kinds."},
		"template_missing":      {"shared template is missing", "Create this namespaced template or correct the templateRef name."},
		"template_yaml":         {"shared template YAML is invalid", "Correct the named shared template YAML and retry."},
		"template_permission":   {"shared template read is not allowed", "Ask the operator to allow template reads in the configured namespace."},
		"certificate":           {"template certificate is not trusted", "Configure the correct CA, name and certificate lifetime for the template reader."},
		"field_type":            {"field value has the wrong native type", "Use the native field type; quote container arguments that are numbers."},
		"unknown_field":         {"unknown field; field names are case-sensitive", "Use the exact native spelling of this field."},
		"local_template":        {"template is not defined in this file", "Correct the template name or add a template with that name."},
		"image":                 {"image is outside the allowed registries", "Use an image under an allowed prefix or ask the operator to allow this registry."},
		"http":                  {"HTTP templates are not supported", "Call the endpoint from a container step instead."},
		"plugin":                {"plugin templates are not supported", "Call the endpoint from a container step instead."},
		"cluster_kind":          {"cluster-scoped kinds cannot be changed by a pipeline", "Grant only namespaced resource kinds."},
		"projection":            {"projected source type is not supported", "Use downward API, a granted Secret or ConfigMap, or an Oberth token projection."},
		"volume":                {"volume source type is not supported", "Use a granted PVC instead."},
		"workload_kind":         {"resource grant includes an Argo workload kind", "Remove Argo API kinds from this resource grant."},
		"namespace_policy":      {"protected namespaces are not configured", "Configure the server, Argo and pipeline namespaces before granting resource access."},
		"protected_namespace":   {"resource grant targets a protected namespace", "Grant a deployment namespace outside the server, Argo and pipeline namespaces."},
		"document":              {"one strict native Workflow is required", "Correct the YAML fields and provide exactly one argoproj.io/v1alpha1 Workflow."},
		"depth":                 {"document or template nesting exceeds 64 levels", "Reduce document, inline and shared template nesting to at most 64 levels."},
		"release_parameter":     {"release parameter belongs to the server", "Remove this argument or global output; a verified release supplies its value."},
		"reserved":              {"field belongs to Oberth or the native executor", "Remove this field or rename it outside the reserved names."},
		"host":                  {"field grants access to the node", "Remove host access and use Pod-local storage, networking and containers."},
		"patch":                 {"Pod patch can replace the assigned identity", "Remove podSpecPatch and declare native fields directly."},
		"identity":              {"verified execution accounts are required", "Ask the operator to configure and verify the pipeline and resource accounts."},
		"reference":             {"named namespaced template reference is required", "Use a literal namespaced templateRef and an existing named template."},
		"cycle":                 {"shared template references contain a cycle", "Break the cycle containing a templateRef edge; local recursion is allowed."},
		"live":                  {"live authorization evidence is required", "Run oberth check --repo <repo> <ref>; ask the operator to supply the reported account, template or grant."},
		"binding":               {"run or source binding does not match the admitted input", "Prepare the exact repository, file, commit and source declarations again."},
		"source":                {"source input declaration is invalid", "Use plugin oberth, the reserved key, an exact descriptor, mode 0755 and pin names of at most 128 bytes."},
		"source_usage":          {"working directory requires the main source input", "Declare or forward the main source input, or cover the working directory with your own volume."},
		"source_result":         {"result of a source step is not readable", "Write the value to a file and use an output parameter."},
		"artifact":              {"artifact origin or destination is unsafe", "Use an ordinary artifact location or the source plugin, and literal paths outside protected credentials."},
		"path":                  {"path is reserved, unclean or overlaps source", "Choose a clean path somewhere else, outside reserved paths and source mounts."},
		"audience":              {"identity token audience is empty", "Set an explicit audience and ask the operator to grant that exact audience."},
		"token":                 {"credential projection must be read-only in the main container", "Use one granted audience, 600–3600 seconds, and one read-only main mount; keep helpers separate."},
		"resource_namespace":    {"resource manifest has no namespace", "Set metadata.namespace explicitly to the granted deployment namespace."},
		"capability":            {"resource object, namespace or action is not granted", "Grant the exact namespace, action and account; use at most 32 kinds with names of at most 63 bytes."},
		"resource":              {"resource manifest envelope is invalid", "Use one inline object with exact envelope keys and only Workflow identity substitutions."},
		"schedule":              {"schedule entry must match the exact run trigger", "Use a literal entry of at most 64 bytes only for a schedule trigger."},
	}
}

func finding(path, code string) admissioncontract.Finding {
	m := messages()[code]
	return admissioncontract.Finding{Path: path, Code: code, Reason: m.reason, Remedy: m.remedy}
}

// AdmissionError lists every independently diagnosable refusal in traversal order.
type AdmissionError struct {
	cause    error
	Findings []admissioncontract.Finding
	Needs    []admissioncontract.Finding
	Detail   string
}

func (e *AdmissionError) Error() string {
	lines := make([]string, 0, len(e.Findings))
	for _, f := range e.Findings {
		fix := f.Remedy
		if f.Command != "" {
			fix = f.Command
		}
		lines = append(lines, fmt.Sprintf("%s: %s; %s", f.Path, f.Reason, fix))
	}
	if e.Detail != "" {
		lines = append(lines, e.Detail)
	}
	return strings.Join(lines, "\n")
}
func (e *AdmissionError) Unwrap() error  { return e.cause }
func (e *AdmissionError) Reason() string { return "Workflow admission refused." }
func (e *AdmissionError) Remedy() string { return "Correct every listed field and push again." }

// TemplateReadError keeps failed live I/O distinct from an authored refusal.
type TemplateReadError struct {
	Name  string
	Path  string
	Cause error
}

func (e *TemplateReadError) Error() string {
	return e.Path + ": template " + e.Name + ": " + e.Reason() + "; " + e.Remedy()
}
func (e *TemplateReadError) Reason() string { return "shared template read failed" }
func (e *TemplateReadError) Remedy() string { return "Retry when the template reader is available." }
func (e *TemplateReadError) Unwrap() error  { return e.Cause }

func refusal(path, code string) error {
	return &AdmissionError{Findings: []admissioncontract.Finding{finding(path, code)}}
}

func templateReadFailure(name, path string, cause error) error {
	var authored *AdmissionError
	if errors.As(cause, &authored) {
		result := &AdmissionError{cause: cause, Detail: "template " + name + ": " + authored.Detail}
		for _, f := range authored.Findings {
			f.Path = path + ".template." + f.Path
			result.Findings = append(result.Findings, f)
		}
		return result
	}
	code := ""
	switch {
	case apierrors.IsNotFound(cause):
		code = "template_missing"
	case apierrors.IsForbidden(cause) || apierrors.IsUnauthorized(cause):
		code = "template_permission"
	case certificateFailure(cause):
		code = "certificate"
	case templateYAMLFailure(cause):
		code = "template_yaml"
	default:
		return &TemplateReadError{Name: name, Path: path, Cause: cause}
	}
	return &AdmissionError{Findings: []admissioncontract.Finding{finding(path, code)}, Detail: "template " + name + ": " + cause.Error(), cause: cause}
}
func certificateFailure(cause error) bool {
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certificate x509.CertificateInvalidError
	return errors.As(cause, &authority) || errors.As(cause, &hostname) || errors.As(cause, &certificate)
}
func templateYAMLFailure(cause error) bool {
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	var yaml *yamlv3.TypeError
	return errors.As(cause, &syntax) || errors.As(cause, &typed) || errors.As(cause, &yaml)
}
func grantCommand(key workflowpolicy.GrantKey, kind string, release bool) string {
	if key.Repository == "" {
		key.Repository = "<repo>"
	}
	grant := publicerr.Grant{Repo: key.Repository, File: key.File, Step: key.Step, Path: key.Path, Audience: key.Audience}
	switch kind {
	case "pvc":
		return publicerr.ClaimAllowCommand(key.Repository, key.Path, release)
	case "secret":
		return publicerr.SecretPutCommand(grant)
	}
	return publicerr.AccessAllowCommand(kind, grant)
}

func fileError(file string, err error) error {
	if err == nil || file == "" {
		return err
	}
	return fmt.Errorf("%s: %w", file, err)
}
