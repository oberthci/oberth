package argojob

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const (
	trustedKVMTemplate   = "oberth-trusted-tests"
	trustedKVMWrapper    = "oberth-trusted-preflight"
	trustedKVMArtifacts  = "oberth-trusted-artifacts"
	trustedKVMPrivate    = "oberth-trusted-private"
	trustedKVMAnnotation = "oberth.ci/trusted-kvm-intent"
)

func prepareTrustedKVMRequest(config Config, request *Request) error {
	intent := request.TrustedKVM
	if !config.KVMEnabled {
		return ErrKVMNotEnabled
	}
	if config.TrustedKVMGroup <= 0 {
		return errors.New("argojob: trusted suites require an explicit positive KVM supplemental group")
	}
	digest := sha256.Sum256(request.Source)
	if intent.Validate() != nil || request.Trigger != periapsis.TriggerCI || request.RunID != intent.RunID || request.Name != intent.WorkflowName || request.SHA != intent.CandidateSHA || config.Namespace != intent.Namespace || hex.EncodeToString(digest[:]) != intent.WorkflowSpecDigest || request.UpstreamName+"/"+request.UpstreamOrg+"/"+request.Repo != intent.Repository || len(request.Fragments) != 0 {
		return errors.New("argojob: trusted preflight request differs from journaled admission")
	}
	expected := SourceVolume{ClaimName: sourceClaimName(request.Name), SubPath: "src", ArtifactsSubPath: artifactsSubPath}
	if config.GoProxyCACertPEM != "" {
		expected.GoProxyCASubPath = goProxyCASubPath + "/" + goProxyCACertFile
	}
	if request.SourceVolume.ClaimName != "" && !reflect.DeepEqual(request.SourceVolume, expected) {
		return errors.New("argojob: trusted preflight requires its exact credential-free source volume")
	}
	request.SourceVolume = expected
	return nil
}

// The first closed profile supports ordinary producers, but no background,
// retry, exit-hook or cached execution that could overlap its protected leaf.
func admitTrustedKVMSource(workflow *wfv1.Workflow) error {
	body, err := json.Marshal(workflow.Spec)
	if err != nil {
		return err
	}
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return err
	}
	forbidden := map[string]bool{"templateDefaults": true, "hooks": true, "onExit": true, "daemon": true, "retryStrategy": true, "memoize": true, "templateRef": true, "workflowTemplateRef": true, "podSpecPatch": true, "artifactRepositoryRef": true, "archiveLogs": true, "artifactGC": true, "suspend": true}
	var visit func(any) error
	visit = func(node any) error {
		switch value := node.(type) {
		case map[string]any:
			for key, child := range value {
				if forbidden[key] {
					return fmt.Errorf("argojob: trusted preflight does not admit %s", key)
				}
				if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range value {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(document); err != nil {
		return err
	}
	if workflow.Annotations[secretPathsAnnotation] != "" || len(workflow.Spec.Arguments.Artifacts) > 0 {
		return errors.New("argojob: trusted preflight cannot carry credentials or external artifacts")
	}
	for key := range workflow.Annotations {
		if key == trustedKVMAnnotation || strings.HasPrefix(key, "oberth.ci/release-wif") {
			return errors.New("argojob: trusted preflight reserved annotation or WIF declaration")
		}
	}
	for _, claim := range workflow.Spec.VolumeClaimTemplates {
		if trustedKVMReservedVolume(claim.Name) {
			return errors.New("argojob: trusted preflight reserved claim")
		}
	}
	var result error
	walkTemplates(workflow, func(t *wfv1.Template) {
		for key := range t.Metadata.Annotations {
			if strings.HasPrefix(key, "oberth.ci/release-wif") {
				result = errors.New("argojob: trusted preflight refuses WIF")
			}
		}
		if t.Name == trustedKVMTemplate || t.Name == trustedKVMWrapper || t.Resource != nil || t.Data != nil || t.HTTP != nil || t.Plugin != nil || len(t.Inputs.Artifacts) > 0 || len(t.Outputs.Artifacts) > 0 {
			result = errors.New("argojob: trusted preflight reserved template or unsupported producer")
		}
		for _, v := range t.Volumes {
			if trustedKVMReservedVolume(v.Name) {
				result = errors.New("argojob: trusted preflight reserved volume")
			}
		}
	})
	for _, v := range workflow.Spec.Volumes {
		if trustedKVMReservedVolume(v.Name) {
			return errors.New("argojob: trusted preflight reserved volume")
		}
	}
	return result
}
func appendTrustedKVM(workflow *wfv1.Workflow, config Config, request Request) error {
	intent := request.TrustedKVM
	if workflow.Spec.ServiceAccountName != config.PipelineServiceAccount || workflow.Spec.AutomountServiceAccountToken == nil || *workflow.Spec.AutomountServiceAccountToken {
		return errors.New("argojob: trusted preflight requires the credential-free pipeline identity")
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	leaf := wfv1.Template{
		Name:               trustedKVMTemplate,
		SecurityContext:    trustedKVMPodSecurity(config.TrustedKVMGroup),
		ServiceAccountName: config.PipelineServiceAccount, AutomountServiceAccountToken: ptr.To(false), Executor: &wfv1.ExecutorConfig{ServiceAccountName: config.ExecutorServiceAccount},
		NodeSelector: map[string]string{"kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux"},
		Volumes: []corev1.Volume{
			{Name: trustedKVMArtifacts, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: request.SourceVolume.ClaimName, ReadOnly: true}}},
			{Name: trustedKVMPrivate, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("8Gi"))}}},
		},
		Container: &corev1.Container{
			Image: intent.Recipe.HarnessImage, Command: []string{"/usr/local/bin/oberth-kvm-harness"}, Args: []string{"--intent", base64.RawURLEncoding.EncodeToString(encoded)}, WorkingDir: "/trusted",
			Env:             []corev1.EnvVar{{Name: "OBERTH_KVM", Value: "1"}},
			Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourceName(kvmDeviceResource): resource.MustParse("1")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourceName(kvmDeviceResource): resource.MustParse("1")}},
			VolumeMounts:    []corev1.VolumeMount{{Name: trustedKVMArtifacts, MountPath: ArtifactsMountPath, SubPath: artifactsSubPath, ReadOnly: true}, {Name: trustedKVMPrivate, MountPath: "/trusted"}},
			SecurityContext: nonrootContainerSecurity(nonrootUID), TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		},
	}
	// Argo's wait executor mirrors main mounts. The server deletes those mirrors;
	// neither executor credentials nor helpers may share the private guest state.
	securityBody, _ := json.Marshal(nonrootContainerSecurity(0))
	var helperSecurity map[string]any
	if err := json.Unmarshal(securityBody, &helperSecurity); err != nil {
		return err
	}
	helperSecurity["$patch"] = "replace"
	patch := map[string]any{
		"initContainers": []any{map[string]any{"name": "init", "securityContext": helperSecurity}},
		"containers":     []any{map[string]any{"name": "wait", "securityContext": helperSecurity, "volumeMounts": []any{map[string]any{"mountPath": "/mainctrfs/work/artifacts", "$patch": "delete"}, map[string]any{"mountPath": "/mainctrfs/trusted", "$patch": "delete"}}}},
	}

	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	leaf.PodSpecPatch = string(body)
	wrapper := wfv1.Template{Name: trustedKVMWrapper, Steps: []wfv1.ParallelSteps{
		{Steps: []wfv1.WorkflowStep{{Name: "candidate", Template: workflow.Spec.Entrypoint, Arguments: workflow.Spec.Arguments}}},
		{Steps: []wfv1.WorkflowStep{{Name: "trusted-tests", Template: trustedKVMTemplate}}},
	}}
	workflow.Spec.Templates = append(workflow.Spec.Templates, leaf, wrapper)
	workflow.Spec.Entrypoint = trustedKVMWrapper
	// A preflight's admission deadline bounds producers and the suite together.
	seconds := int64(intent.Deadline.Sub(intent.AdmittedAt) / time.Second)
	if seconds < 1 {
		return errors.New("argojob: trusted preflight deadline below one second")
	}
	workflow.Spec.ActiveDeadlineSeconds = &seconds
	workflow.Annotations[trustedKVMAnnotation] = vmrunner.KVMIntentDigest(*intent)
	return nil
}

func trustedKVMReservedVolume(name string) bool {
	switch name {
	case trustedKVMArtifacts, trustedKVMPrivate, "var-run-argo", "tmp-dir-argo", executorTokenVolume:
		return true
	}
	return false
}

func trustedKVMPodSecurity(kvmGroup int64) *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsUser: ptr.To(nonrootUID), RunAsGroup: ptr.To(nonrootUID), RunAsNonRoot: ptr.To(true),
		FSGroup: ptr.To(nonrootUID), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
		SupplementalGroups: []int64{kvmGroup}, SupplementalGroupsPolicy: ptr.To(corev1.SupplementalGroupsPolicyStrict),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}
