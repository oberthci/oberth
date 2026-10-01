package argoworkflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

// NonrootTemplatesAnnotation selects the server's fixed, uncredentialed
// UID/GID 65534 execution mode. It never grants a repository securityContext.
const NonrootTemplatesAnnotation = "oberth.ci/nonroot-templates"

// MaxNonrootTemplateBytes bounds this mode's final serialized template.
// Argo v4.0.8 offloads template JSON/arguments above 128KiB into a ConfigMap
// mounted into every init. This static mode permits at most half that size,
// rechecked after server injection, to keep the preparer EmptyDir-only.
const MaxNonrootTemplateBytes = 64 << 10

// ValidateNonrootTemplateSize is shared by admission and final server output.
func ValidateNonrootTemplateSize(tmpl *wfv1.Template) error {
	encoded, err := json.Marshal(tmpl)
	if err != nil {
		return fmt.Errorf("argoworkflow: encode nonroot template: %w", err)
	}
	if len(encoded) > MaxNonrootTemplateBytes {
		return fmt.Errorf("argoworkflow: nonroot template %q exceeds %d bytes; executor ConfigMap offload is not supported", tmpl.Name, MaxNonrootTemplateBytes)
	}
	if strings.Contains(string(encoded), "{{") {
		return fmt.Errorf("argoworkflow: nonroot template %q must be static; Argo substitutions are not supported", tmpl.Name)
	}
	return nil
}

// DeclaredNonrootTemplates validates the deliberately small initial surface:
// named top-level container/script leaves with no inherited Pod shape. The
// server must be able to enumerate every executor container and scratch mount.
func DeclaredNonrootTemplates(workflow *wfv1.Workflow) ([]string, error) {
	if workflow == nil {
		return nil, errors.New("argoworkflow: no Workflow to read")
	}
	value, declared := workflow.Annotations[NonrootTemplatesAnnotation]
	if !declared {
		return nil, nil
	}
	fields := strings.Split(value, ",")
	if len(fields) > MaxTemplates {
		return nil, fmt.Errorf("argoworkflow: %s exceeds %d templates", NonrootTemplatesAnnotation, MaxTemplates)
	}
	paths, err := DeclaredSecretPaths(workflow)
	if err != nil {
		return nil, err
	}
	if len(paths) != 0 {
		return nil, errors.New("argoworkflow: nonroot leaves are not admitted in credentialed workflows")
	}
	if workflow.Spec.TemplateDefaults != nil {
		return nil, errors.New("argoworkflow: nonroot leaves cannot inherit templateDefaults")
	}
	if workflow.Spec.ArchiveLogs != nil && *workflow.Spec.ArchiveLogs {
		return nil, errors.New("argoworkflow: nonroot leaves cannot inherit archiveLogs or artifact executor mounts")
	}
	reserved := map[string]bool{"oberth-nonroot-tmp": true, "var-run-argo": true, "tmp-dir-argo": true, "argo-staging": true}
	for _, volume := range workflow.Spec.Volumes {
		if reserved[volume.Name] {
			return nil, fmt.Errorf("argoworkflow: nonroot executor volume %q is server-owned", volume.Name)
		}
	}
	for _, claim := range workflow.Spec.VolumeClaimTemplates {
		if reserved[claim.Name] {
			return nil, fmt.Errorf("argoworkflow: nonroot executor volume %q cannot be a claim", claim.Name)
		}
	}
	templates := make(map[string]*wfv1.Template, len(workflow.Spec.Templates))
	for i := range workflow.Spec.Templates {
		templates[workflow.Spec.Templates[i].Name] = &workflow.Spec.Templates[i]
	}
	seen := make(map[string]bool, len(fields))
	for i, field := range fields {
		name := strings.TrimSpace(field)
		if len(k8svalidation.IsDNS1123Label(name)) != 0 || seen[name] {
			return nil, fmt.Errorf("argoworkflow: %s has invalid or duplicate template %q", NonrootTemplatesAnnotation, name)
		}
		tmpl := templates[name]
		if tmpl == nil {
			return nil, fmt.Errorf("argoworkflow: %s names unknown top-level template %q", NonrootTemplatesAnnotation, name)
		}
		for _, volume := range tmpl.Volumes {
			if reserved[volume.Name] {
				return nil, fmt.Errorf("argoworkflow: nonroot executor volume %q is server-owned", volume.Name)
			}
		}
		if (tmpl.Container == nil) == (tmpl.Script == nil) || tmpl.ContainerSet != nil ||
			tmpl.DAG != nil || len(tmpl.Steps) != 0 || len(tmpl.InitContainers) != 0 ||
			len(tmpl.Sidecars) != 0 || tmpl.Daemon != nil || tmpl.Resource != nil ||
			tmpl.Data != nil || tmpl.Suspend != nil || tmpl.HTTP != nil || tmpl.Plugin != nil ||
			len(tmpl.Inputs.Artifacts) != 0 || len(tmpl.Outputs.Artifacts) != 0 || tmpl.ArchiveLocation != nil {
			return nil, fmt.Errorf("argoworkflow: nonroot template %q must be a plain container or script leaf without extra executors or artifacts", name)
		}
		container := tmpl.Container
		if tmpl.Script != nil {
			container = &tmpl.Script.Container
		}
		if container.Name != "" && container.Name != "main" {
			return nil, fmt.Errorf("argoworkflow: nonroot template %q must omit the main container name or use main", name)
		}
		if len(tmpl.Inputs.Parameters) != 0 {
			return nil, fmt.Errorf("argoworkflow: nonroot template %q cannot receive parameter inputs", name)
		}
		if err := ValidateNonrootTemplateSize(tmpl); err != nil {
			return nil, err
		}
		fields[i], seen[name] = name, true
	}
	return fields, nil
}
