package argojob

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const (
	releaseWIFVolume        = "oberth-release-wif"
	releaseWIFPath          = "/run/oberth-gar"
	releaseWIFADCAnnotation = "oberth.ci/release-wif-adc"
)

// ReleaseWIFConfig contains public authorization data, never private keys.
// Each role uses a distinct pool so a token cannot impersonate another role's
// target merely by replacing the client-side ADC configuration.
type ReleaseWIFConfig struct {
	Namespace    string                          `json:"namespace"`
	Roles        map[string]ReleaseWIFRole       `json:"roles"`
	Repositories map[string]ReleaseWIFRepository `json:"repositories"`
}

// Clone freezes the nested capability maps at engine construction. A caller's
// later edits cannot change either admission or the controller's retry Build.
func (config *ReleaseWIFConfig) Clone() *ReleaseWIFConfig {
	if config == nil {
		return nil
	}
	copy := *config
	copy.Roles = maps.Clone(config.Roles)
	copy.Repositories = maps.Clone(config.Repositories)
	for repo, grant := range copy.Repositories {
		grant.Templates = maps.Clone(grant.Templates)
		copy.Repositories[repo] = grant
	}
	return &copy
}

type ReleaseWIFRole struct {
	Provider       string `json:"provider"`
	ServiceAccount string `json:"service_account"`
}

type ReleaseWIFRepository struct {
	ServiceAccountName string            `json:"service_account_name"`
	Templates          map[string]string `json:"templates"`
}

var wifProviderPattern = regexp.MustCompile(`^projects/[0-9]+/locations/global/workloadIdentityPools/[a-z][a-z0-9-]*/providers/[a-z][a-z0-9-]*$`)
var wifAccountPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*@[a-z][a-z0-9-]*\.iam\.gserviceaccount\.com$`)

func (config *ReleaseWIFConfig) Validate() error {
	if config == nil {
		return nil
	}
	if (len(config.Roles) > 0 || len(config.Repositories) > 0) && len(k8svalidation.IsDNS1123Label(config.Namespace)) != 0 {
		return errors.New("argojob: release WIF requires an exact pipeline namespace")
	}
	pools := map[string]bool{}
	accounts := map[string]bool{}
	for name, role := range config.Roles {
		if !argoworkflow.ValidReleaseWIFRole(name) || !wifProviderPattern.MatchString(role.Provider) || !wifAccountPattern.MatchString(role.ServiceAccount) {
			return fmt.Errorf("argojob: invalid administrator release WIF role %q", name)
		}
		pool := strings.Split(role.Provider, "/providers/")[0]
		if pools[pool] {
			return errors.New("argojob: release WIF roles must use distinct workload identity pools")
		}
		if accounts[role.ServiceAccount] {
			return errors.New("argojob: release WIF roles must use distinct target service accounts")
		}
		pools[pool] = true
		accounts[role.ServiceAccount] = true
	}
	subjects := map[string]bool{}
	for repo, grant := range config.Repositories {
		upstream, org, name, err := gitcache.ParseRepoPath(repo)
		if err != nil || upstream == "" || gitcache.ValidateUpstreamName(upstream) != nil || repo != upstream+"/"+org+"/"+name || len(k8svalidation.IsDNS1123Subdomain(grant.ServiceAccountName)) != 0 || subjects[grant.ServiceAccountName] {
			return fmt.Errorf("argojob: release WIF requires an exact canonical repository and ServiceAccount: %q", repo)
		}
		subjects[grant.ServiceAccountName] = true
		for leaf, role := range grant.Templates {
			if len(k8svalidation.IsDNS1123Label(leaf)) != 0 || config.Roles[role].Provider == "" {
				return fmt.Errorf("argojob: invalid release WIF capability for %q template %q", repo, leaf)
			}
		}
	}
	return nil
}

func authorizeReleaseWIF(wf *wfv1.Workflow, config Config, request Request) ([]*wfv1.Template, error) {
	if err := config.ReleaseWIF.Validate(); err != nil {
		return nil, err
	}
	if _, declared := wf.Annotations["oberth.ci/release-wif-policy-sha256"]; declared {
		return nil, errors.New("argojob: release WIF policy provenance is assigned by Oberth")
	}
	if _, declared := wf.Annotations[argoworkflow.ReleaseWIFRoleAnnotation]; declared {
		return nil, errors.New("argojob: release WIF must be declared on named Pod leaves, not the Workflow")
	}
	if d := wf.Spec.TemplateDefaults; d != nil {
		if _, declared := d.Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation]; declared {
			return nil, errors.New("argojob: release WIF must be declared on named Pod leaves, not templateDefaults")
		}
	}
	var leaves []*wfv1.Template
	var failure error
	top := map[*wfv1.Template]bool{}
	for i := range wf.Spec.Templates {
		top[&wf.Spec.Templates[i]] = true
	}
	walkTemplates(wf, func(t *wfv1.Template) {
		role, declared := t.Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation]
		if !declared || failure != nil {
			return
		}
		if request.Trigger != periapsis.TriggerRelease || config.ReleaseWIF == nil || config.ReleaseWIF.Namespace != config.Namespace {
			failure = errors.New("argojob: release WIF requires an enabled administrator capability and a release trigger")
			return
		}
		key := canonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repo)
		grant, ok := config.ReleaseWIF.Repositories[key]
		identity, hasIdentity := config.identityForPerRepo(request.UpstreamName, request.UpstreamOrg, request.Repo)
		if grant.ServiceAccountName == config.PipelineServiceAccount || grant.ServiceAccountName == config.CredentialedServiceAccount || grant.ServiceAccountName == config.CISecretsServiceAccount || grant.ServiceAccountName == config.ExecutorServiceAccount {
			hasIdentity = false
		}
		for otherRepo, other := range config.PerRepoIdentities {
			if otherRepo != key && other.ServiceAccountName == grant.ServiceAccountName {
				hasIdentity = false
			}
		}
		for _, other := range config.PerRepoCIIdentities {
			if other.ServiceAccountName == grant.ServiceAccountName {
				hasIdentity = false
			}
		}
		if !ok || !hasIdentity || identity.ServiceAccountName != grant.ServiceAccountName || grant.Templates[t.Name] != role || !argoworkflow.ValidReleaseWIFRole(role) {
			failure = fmt.Errorf("argojob: release WIF capability is not approved for %q template %q role %q", key, t.Name, role)
			return
		}
		if !top[t] || (t.Container == nil && t.Script == nil) || t.ContainerSet != nil || wf.Spec.TemplateDefaults != nil || len(t.Inputs.Artifacts) > 0 || len(t.Outputs.Artifacts) > 0 || wf.Annotations[argoworkflow.NonrootTemplatesAnnotation] != "" {
			failure = fmt.Errorf("argojob: release WIF template %q must be a named container/script leaf without defaults, artifact plugins, or nonroot mode", t.Name)
			return
		}
		leaves = append(leaves, t)
	})
	if failure != nil || len(leaves) == 0 {
		return leaves, failure
	}
	encoded, err := json.Marshal(config.ReleaseWIF)
	if err != nil {
		return nil, err
	}
	// Public policy provenance is part of the existing immutable spec digest
	// and submission audit binding; no token or derived token digest is stored.
	if wf.Annotations == nil {
		wf.Annotations = make(map[string]string)
	}
	wf.Annotations["oberth.ci/release-wif-policy-sha256"] = fmt.Sprintf("%x", sha256.Sum256(encoded))
	// No authored alias may gain access to the generated projection. Reject
	// before adding it, including helpers and templates that did not opt in.
	checkVolumes := func(volumes []corev1.Volume) {
		for _, v := range volumes {
			if v.Name == releaseWIFVolume {
				failure = errors.New("argojob: release WIF volume is server-owned")
			}
		}
	}
	checkVolumes(wf.Spec.Volumes)
	for _, claim := range wf.Spec.VolumeClaimTemplates {
		if claim.Name == releaseWIFVolume {
			failure = errors.New("argojob: release WIF volume cannot be an authored claim")
		}
	}
	walkTemplates(wf, func(t *wfv1.Template) {
		checkVolumes(t.Volumes)
		templateContainers(t, func(c *corev1.Container) {
			for _, m := range c.VolumeMounts {
				if m.Name == releaseWIFVolume || path.Clean(m.MountPath) == releaseWIFPath || strings.HasPrefix(path.Clean(m.MountPath), releaseWIFPath+"/") {
					failure = errors.New("argojob: release WIF mounts are server-owned")
				}
			}
		})
	})
	return leaves, failure
}

func injectReleaseWIF(wf *wfv1.Workflow, leaves []*wfv1.Template, config *ReleaseWIFConfig) error {
	for _, t := range leaves {
		if tokenDisabled(t) {
			return fmt.Errorf("argojob: release WIF template %q explicitly disables its service-account token", t.Name)
		}
		// This also runs when source/cache injection had no mounts and skipped
		// its normal mirror normalization pass. Never leave Argo a late mirror
		// instruction that could copy the new projection into a helper.
		resolveUserMountMirrors(t)
		// The explicit audience-scoped projection below is the only token
		// this capability adds, even when the live ServiceAccount defaults true.
		t.AutomountServiceAccountToken = ptr.To(false)
		role := config.Roles[t.Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation]]
		// #nosec G101 -- public external_account schema/endpoints and a projected token filename, not credential bytes.
		adc := map[string]any{
			"type": "external_account", "audience": "//iam.googleapis.com/" + role.Provider,
			"subject_token_type":                "urn:ietf:params:oauth:token-type:jwt",
			"token_url":                         "https://sts.googleapis.com/v1/token",
			"service_account_impersonation_url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + role.ServiceAccount + ":generateAccessToken",
			"credential_source":                 map[string]string{"file": releaseWIFPath + "/token"},
		}
		encoded, err := json.Marshal(adc)
		if err != nil {
			return err
		}
		t.Metadata.Annotations[releaseWIFADCAnnotation] = string(encoded)
		t.Volumes = append(t.Volumes, corev1.Volume{Name: releaseWIFVolume, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To(int32(0400)), Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "https://iam.googleapis.com/" + role.Provider, ExpirationSeconds: ptr.To(int64(600)), Path: "token"}},
				{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "adc.json", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.annotations['" + releaseWIFADCAnnotation + "']"}}}}},
			},
		}}})
		main := templateMainContainer(t)
		main.VolumeMounts = append(main.VolumeMounts, corev1.VolumeMount{Name: releaseWIFVolume, MountPath: releaseWIFPath, ReadOnly: true})
		main.Env = overrideEnvironment(main.Env, []corev1.EnvVar{
			{Name: "GOOGLE_APPLICATION_CREDENTIALS", Value: releaseWIFPath + "/adc.json"},
			{Name: "OBERTH_RELEASE_WIF", Value: "1"},
		})
	}
	if len(leaves) > 0 {
		// Selecting a per-repository identity normalized every template's
		// automount field to true, including ordinary unselected siblings.
		// Argo omits true from Pods, letting a live SA default mount its API
		// token into every sibling container. Disable that ambient authority
		// throughout this workflow, after all authored opt-out vetoes and
		// explicit WIF/Vault projections have been applied. The executor's
		// separate bookkeeping token remains unchanged.
		wf.Spec.AutomountServiceAccountToken = ptr.To(false)
		walkTemplates(wf, func(t *wfv1.Template) {
			t.AutomountServiceAccountToken = ptr.To(false)
		})
	}
	return nil
}

func protectReleaseWIFExecutor(leaves []*wfv1.Template) error {
	for _, t := range leaves {
		patch := map[string]any{}
		if t.PodSpecPatch != "" {
			if err := json.Unmarshal([]byte(t.PodSpecPatch), &patch); err != nil {
				return err
			}
		}
		// Strategic merge keys volumeMounts by mountPath. Delete only the
		// controller-generated JWT/ADC mirror, preserving other server policy.
		containers, _ := patch["containers"].([]any)
		var wait map[string]any
		for _, c := range containers {
			m, _ := c.(map[string]any)
			if m["name"] == argoWaitName {
				wait = m
			}
		}
		if wait == nil {
			wait = map[string]any{"name": argoWaitName}
			containers = append(containers, wait)
		}
		mounts, _ := wait["volumeMounts"].([]any)
		filtered := make([]any, 0, len(mounts)+1)
		for _, m := range mounts {
			v, _ := m.(map[string]any)
			if v["mountPath"] != path.Join(argoMainFilesystem, releaseWIFPath) {
				filtered = append(filtered, m)
			}
		}
		wait["volumeMounts"] = append(filtered, map[string]any{"mountPath": path.Join(argoMainFilesystem, releaseWIFPath), "$patch": "delete"})
		patch["containers"] = containers
		encoded, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		t.PodSpecPatch = string(encoded)
	}
	return nil
}
