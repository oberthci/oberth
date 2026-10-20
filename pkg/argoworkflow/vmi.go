package argoworkflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// The manifest is deliberately decoded into this small shape, rather than a
// KubeVirt API object. KubeVirt adds fields over time; a newly added device,
// volume, identity, or host integration must remain refused until reviewed.
type vmiManifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Architecture string `json:"architecture"`
		Domain       struct {
			CPU struct {
				Cores int `json:"cores"`
			} `json:"cpu"`
			// Firmware admits only kernelBoot from an allowlisted, digest-pinned
			// container. Bootloader, serial, UUID and ACPI fields stay refused.
			Firmware *struct {
				KernelBoot *vmiKernelBoot `json:"kernelBoot"`
			} `json:"firmware"`
			Resources struct {
				Requests struct {
					CPU    string `json:"cpu"`
					Memory string `json:"memory"`
				} `json:"requests"`
			} `json:"resources"`
			Devices struct {
				Disks []struct {
					Name string `json:"name"`
					Disk struct {
						Bus string `json:"bus"`
					} `json:"disk"`
				} `json:"disks"`
				Interfaces []struct {
					Name       string    `json:"name"`
					Masquerade *struct{} `json:"masquerade"`
				} `json:"interfaces"`
			} `json:"devices"`
		} `json:"domain"`
		Volumes []struct {
			Name          string `json:"name"`
			ContainerDisk *struct {
				Image string `json:"image"`
			} `json:"containerDisk"`
			CloudInitNoCloud *struct {
				UserData string `json:"userData"`
			} `json:"cloudInitNoCloud"`
			EmptyDisk *struct {
				Capacity string `json:"capacity"`
			} `json:"emptyDisk"`
		} `json:"volumes"`
		Networks []struct {
			Name string    `json:"name"`
			Pod  *struct{} `json:"pod"`
		} `json:"networks"`
	} `json:"spec"`
}

// vmiKernelBoot is KubeVirt's spec.domain.firmware.kernelBoot. Only the image
// and in-image paths are admitted: imagePullSecret would name a Secret.
type vmiKernelBoot struct {
	KernelArgs string `json:"kernelArgs"`
	Container  *struct {
		Image      string `json:"image"`
		KernelPath string `json:"kernelPath"`
		InitrdPath string `json:"initrdPath"`
	} `json:"container"`
}

var vmiNamePattern = regexp.MustCompile(`^\{\{workflow\.name\}\}-[a-z0-9]([a-z0-9-]{0,14}[a-z0-9])?$`)
var vmiDNSLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var yamlDocumentMarker = regexp.MustCompile(`(?m)^(?:---|\.\.\.)(?:[ \t]|$)`)

// kernelArgsPattern bounds the guest kernel command line to plain
// space-separated key[=value] words. It excludes quotes, braces, shell and
// YAML metacharacters, so it cannot carry an Argo tag or a second document.
var kernelArgsPattern = regexp.MustCompile(`^[A-Za-z0-9._,:=/+-]+( [A-Za-z0-9._,:=/+-]+)*$`)
var kernelBootPathPattern = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)

const (
	maxVMIManifestBytes = 128 << 10
	maxVMICores         = 4
	maxVMIMemory        = "8Gi"
	maxVMIEmptyDisk     = "16Gi"
	// Each admitted VM may need its own create, get and delete template.
	vmiTemplatesPerVM  = 3
	maxKernelArgsBytes = 512
	maxKernelPathBytes = 256
)

// VMIBudget bounds what one run's VMI create templates may request together:
// the number of VMs, their summed cores and their summed memory requests. A
// repository's KubeVirt grant carries one; the server's ceiling caps it.
type VMIBudget struct {
	MaxVMs    int
	MaxCores  int
	MaxMemory resource.Quantity
}

// DefaultVMIBudget is the budget of a grant that names no explicit values.
func DefaultVMIBudget() VMIBudget {
	return VMIBudget{MaxVMs: 2, MaxCores: 6, MaxMemory: resource.MustParse("12Gi")}
}

// VMIBudgetLimit is the largest budget a grant or ceiling may name. It keeps
// a typo from becoming an unbounded per-run allowance.
func VMIBudgetLimit() VMIBudget {
	return VMIBudget{MaxVMs: 16, MaxCores: 64, MaxMemory: resource.MustParse("256Gi")}
}

// IsZero reports a budget with no field set, which selects the default.
func (budget VMIBudget) IsZero() bool {
	return budget.MaxVMs == 0 && budget.MaxCores == 0 && budget.MaxMemory.IsZero()
}

// Validate requires every field to be positive and within VMIBudgetLimit.
func (budget VMIBudget) Validate() error {
	limit := VMIBudgetLimit()
	if budget.MaxVMs < 1 || budget.MaxVMs > limit.MaxVMs {
		return fmt.Errorf("VMI budget max VMs must be 1..%d", limit.MaxVMs)
	}
	if budget.MaxCores < 1 || budget.MaxCores > limit.MaxCores {
		return fmt.Errorf("VMI budget max cores must be 1..%d", limit.MaxCores)
	}
	if budget.MaxMemory.Cmp(resource.MustParse("1Gi")) < 0 || budget.MaxMemory.Cmp(limit.MaxMemory) > 0 {
		return fmt.Errorf("VMI budget max memory must be 1Gi..%s", limit.MaxMemory.String())
	}
	return nil
}

// Within returns the field-wise minimum of budget and ceiling.
func (budget VMIBudget) Within(ceiling VMIBudget) VMIBudget {
	effective := budget
	effective.MaxVMs = min(budget.MaxVMs, ceiling.MaxVMs)
	effective.MaxCores = min(budget.MaxCores, ceiling.MaxCores)
	if ceiling.MaxMemory.Cmp(budget.MaxMemory) < 0 {
		effective.MaxMemory = ceiling.MaxMemory
	}
	return effective
}

func (budget VMIBudget) String() string {
	return fmt.Sprintf("%d VMs, %d cores, %s memory", budget.MaxVMs, budget.MaxCores, budget.MaxMemory.String())
}

// effectiveVMIBudget applies the defaults for unset policy fields and the
// ceiling. Admit has validated both before calling it.
func (policy Policy) effectiveVMIBudget() (grant, ceiling, effective VMIBudget) {
	grant, ceiling = policy.VMIBudget, policy.VMICeiling
	if grant.IsZero() {
		grant = DefaultVMIBudget()
	}
	if ceiling.IsZero() {
		ceiling = VMIBudgetLimit()
	}
	return grant, ceiling, grant.Within(ceiling)
}

func admitVMIResource(where string, tmpl *wfv1.ResourceTemplate, prefixes []string) error {
	if tmpl == nil {
		return errors.New("nil resource template")
	}
	if tmpl.Action != "create" && tmpl.Action != "get" && tmpl.Action != "delete" {
		return fmt.Errorf("argoworkflow: %s.action %q is not admitted; use create, get, or delete", where, tmpl.Action)
	}
	if !tmpl.SetOwnerReference {
		return fmt.Errorf("argoworkflow: %s.setOwnerReference must be true", where)
	}
	if tmpl.ManifestFrom != nil || tmpl.MergeStrategy != "" || len(tmpl.Flags) != 0 ||
		tmpl.SuccessCondition != "" || tmpl.FailureCondition != "" {
		return fmt.Errorf("argoworkflow: %s may use only action, inline manifest, and setOwnerReference", where)
	}
	if len(tmpl.Manifest) == 0 || len(tmpl.Manifest) > maxVMIManifestBytes {
		return fmt.Errorf("argoworkflow: %s.manifest must be inline and at most %d bytes", where, maxVMIManifestBytes)
	}
	if strings.Contains(tmpl.Manifest, "\r") {
		return fmt.Errorf("argoworkflow: %s.manifest must not contain carriage returns", where)
	}
	// Argo substitutes parameters after this admission check. Only the exact
	// workflow name in metadata.name and the run label is permitted to vary.
	if strings.Contains(tmpl.Manifest, "{{=") || strings.Count(tmpl.Manifest, "{{workflow.name}}") != 2 ||
		strings.Contains(strings.ReplaceAll(tmpl.Manifest, "{{workflow.name}}", ""), "{{") {
		return fmt.Errorf("argoworkflow: %s.manifest permits only two {{workflow.name}} substitutions in metadata.name and the run label", where)
	}
	if yamlDocumentMarker.MatchString(tmpl.Manifest) {
		return fmt.Errorf("argoworkflow: %s.manifest must contain one YAML document", where)
	}
	converted, err := yaml.YAMLToJSONStrict([]byte(tmpl.Manifest))
	if err != nil {
		return fmt.Errorf("argoworkflow: %s.manifest: %w", where, err)
	}
	var manifest vmiManifest
	decoder := json.NewDecoder(bytes.NewReader(converted))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("argoworkflow: %s.manifest contains an unapproved field or value: %w", where, err)
	}
	if manifest.APIVersion != "kubevirt.io/v1" || manifest.Kind != "VirtualMachineInstance" {
		return fmt.Errorf("argoworkflow: %s.manifest must be a kubevirt.io/v1 VirtualMachineInstance", where)
	}
	if !vmiNamePattern.MatchString(manifest.Metadata.Name) {
		return fmt.Errorf("argoworkflow: %s.manifest.metadata.name must be {{workflow.name}}-<suffix>", where)
	}
	if len(manifest.Metadata.Labels) != 1 || manifest.Metadata.Labels["oberth.ci/workflow"] != "{{workflow.name}}" {
		return fmt.Errorf("argoworkflow: %s.manifest.metadata.labels must contain only oberth.ci/workflow: {{workflow.name}}", where)
	}
	if tmpl.Action != "create" {
		if !reflect.DeepEqual(manifest.Spec, (vmiManifest{}).Spec) {
			return fmt.Errorf("argoworkflow: %s.manifest.spec must be absent for %s", where, tmpl.Action)
		}
		return nil
	}
	if len(prefixes) == 0 {
		return fmt.Errorf("argoworkflow: %s has no administrator containerDisk registry allowlist", where)
	}
	if err := validateContainerDiskPrefixes(prefixes); err != nil {
		return fmt.Errorf("argoworkflow: %s: %w", where, err)
	}
	if manifest.Spec.Architecture != "amd64" && manifest.Spec.Architecture != "arm64" {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.architecture must be amd64 or arm64", where)
	}
	if manifest.Spec.Domain.CPU.Cores < 1 || manifest.Spec.Domain.CPU.Cores > maxVMICores {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.domain.cpu.cores must be 1..%d", where, maxVMICores)
	}
	cpu, err := resource.ParseQuantity(manifest.Spec.Domain.Resources.Requests.CPU)
	if err != nil || cpu.Sign() <= 0 || cpu.Cmp(resource.MustParse("4")) > 0 {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.domain.resources.requests.cpu must be 1..4", where)
	}
	memory, err := resource.ParseQuantity(manifest.Spec.Domain.Resources.Requests.Memory)
	if err != nil || memory.Sign() <= 0 || memory.Cmp(resource.MustParse(maxVMIMemory)) > 0 {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.domain.resources.requests.memory must be positive and at most %s", where, maxVMIMemory)
	}
	// The run budget counts cores; a CPU request above the vCPU count would
	// reserve node capacity the budget never sees.
	if cpu.Cmp(*resource.NewQuantity(int64(manifest.Spec.Domain.CPU.Cores), resource.DecimalSI)) > 0 {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.domain.resources.requests.cpu must not exceed spec.domain.cpu.cores", where)
	}
	if manifest.Spec.Domain.Firmware != nil {
		if err := admitKernelBoot(where+".manifest.spec.domain.firmware", manifest.Spec.Domain.Firmware.KernelBoot, prefixes); err != nil {
			return err
		}
	}
	if len(manifest.Spec.Volumes) == 0 || len(manifest.Spec.Volumes) > 8 {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.volumes must contain 1..8 volumes", where)
	}
	volumeNames := map[string]bool{}
	containerDisks := 0
	totalEmptyDisk := resource.MustParse("0")
	for i, volume := range manifest.Spec.Volumes {
		path := fmt.Sprintf("%s.manifest.spec.volumes[%d]", where, i)
		if !vmiDNSLabelPattern.MatchString(volume.Name) || volumeNames[volume.Name] {
			return fmt.Errorf("argoworkflow: %s.name must be a unique DNS label", path)
		}
		volumeNames[volume.Name] = true
		kinds := 0
		if volume.ContainerDisk != nil {
			kinds++
			containerDisks++
			image := volume.ContainerDisk.Image
			if err := periapsis.ValidateRunnerImage(image); err != nil {
				return fmt.Errorf("argoworkflow: %s.containerDisk.image: %w", path, err)
			}
			if !periapsis.RunnerImageAllowed(image, prefixes) {
				return fmt.Errorf("argoworkflow: %s.containerDisk.image must have an allowlisted prefix", path)
			}
		}
		if volume.CloudInitNoCloud != nil {
			kinds++
			if len(volume.CloudInitNoCloud.UserData) == 0 || len(volume.CloudInitNoCloud.UserData) > 64<<10 ||
				strings.Contains(volume.CloudInitNoCloud.UserData, "{{") {
				return fmt.Errorf("argoworkflow: %s.cloudInitNoCloud.userData must be inline, static, and at most 64 KiB", path)
			}
		}
		if volume.EmptyDisk != nil {
			kinds++
			capacity, err := resource.ParseQuantity(volume.EmptyDisk.Capacity)
			if err != nil || capacity.Sign() <= 0 || capacity.Cmp(resource.MustParse(maxVMIEmptyDisk)) > 0 {
				return fmt.Errorf("argoworkflow: %s.emptyDisk.capacity must be positive and at most %s", path, maxVMIEmptyDisk)
			}
			totalEmptyDisk.Add(capacity)
			if totalEmptyDisk.Cmp(resource.MustParse(maxVMIEmptyDisk)) > 0 {
				return fmt.Errorf("argoworkflow: %s.manifest.spec.volumes total emptyDisk capacity must be at most %s", where, maxVMIEmptyDisk)
			}
		}
		if kinds != 1 {
			return fmt.Errorf("argoworkflow: %s must use exactly one of containerDisk, cloudInitNoCloud, or emptyDisk", path)
		}
	}
	if containerDisks == 0 {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.volumes requires a containerDisk", where)
	}
	if len(manifest.Spec.Domain.Devices.Disks) != len(volumeNames) {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.domain.devices.disks must name every volume exactly once", where)
	}
	seenDisks := map[string]bool{}
	for i, disk := range manifest.Spec.Domain.Devices.Disks {
		if !vmiDNSLabelPattern.MatchString(disk.Name) {
			return fmt.Errorf("argoworkflow: %s.manifest.spec.domain.devices.disks[%d].name must be a DNS label", where, i)
		}
		if !volumeNames[disk.Name] || seenDisks[disk.Name] || disk.Disk.Bus != "virtio" {
			return fmt.Errorf("argoworkflow: %s.manifest.spec.domain.devices.disks[%d] must name a unique volume with virtio bus", where, i)
		}
		seenDisks[disk.Name] = true
	}
	if len(manifest.Spec.Networks) != 1 || len(manifest.Spec.Domain.Devices.Interfaces) != 1 {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.networks and domain.devices.interfaces must define one matching pod/masquerade network", where)
	}
	if !vmiDNSLabelPattern.MatchString(manifest.Spec.Networks[0].Name) ||
		!vmiDNSLabelPattern.MatchString(manifest.Spec.Domain.Devices.Interfaces[0].Name) {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.networks and domain.devices.interfaces names must be DNS labels", where)
	}
	if manifest.Spec.Networks[0].Name != manifest.Spec.Domain.Devices.Interfaces[0].Name {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.networks and domain.devices.interfaces must define one matching pod/masquerade network", where)
	}
	if manifest.Spec.Networks[0].Pod == nil || manifest.Spec.Domain.Devices.Interfaces[0].Masquerade == nil {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.networks and domain.devices.interfaces require pod and masquerade", where)
	}
	return nil
}

// admitKernelBoot admits KubeVirt's direct kernel boot from a container that
// holds only the kernel and initrd. The image obeys the containerDisk rules:
// digest-pinned and under an administrator prefix. The paths are absolute and
// clean inside that image; the command line is bounded and inert to Argo.
func admitKernelBoot(path string, kernelBoot *vmiKernelBoot, prefixes []string) error {
	if kernelBoot == nil {
		return fmt.Errorf("argoworkflow: %s admits only kernelBoot", path)
	}
	path += ".kernelBoot"
	if kernelBoot.Container == nil {
		return fmt.Errorf("argoworkflow: %s.container is required", path)
	}
	container := kernelBoot.Container
	if err := periapsis.ValidateRunnerImage(container.Image); err != nil {
		return fmt.Errorf("argoworkflow: %s.container.image: %w", path, err)
	}
	if !periapsis.RunnerImageAllowed(container.Image, prefixes) {
		return fmt.Errorf("argoworkflow: %s.container.image must have an allowlisted containerDisk prefix", path)
	}
	if container.KernelPath == "" {
		return fmt.Errorf("argoworkflow: %s.container.kernelPath is required", path)
	}
	for _, field := range []struct{ name, value string }{{"kernelPath", container.KernelPath}, {"initrdPath", container.InitrdPath}} {
		if field.value == "" {
			continue
		}
		segments := strings.Split(field.value, "/")
		if len(field.value) > maxKernelPathBytes || !kernelBootPathPattern.MatchString(field.value) ||
			slices.Contains(segments, "..") || slices.Contains(segments, ".") {
			return fmt.Errorf("argoworkflow: %s.container.%s must be an absolute path of at most %d bytes using [A-Za-z0-9._-] segments and no . or .. segment", path, field.name, maxKernelPathBytes)
		}
	}
	if kernelBoot.KernelArgs != "" &&
		(len(kernelBoot.KernelArgs) > maxKernelArgsBytes || !kernelArgsPattern.MatchString(kernelBoot.KernelArgs)) {
		return fmt.Errorf("argoworkflow: %s.kernelArgs must be at most %d bytes of single-space-separated [A-Za-z0-9._,:=/+-] words", path, maxKernelArgsBytes)
	}
	return nil
}

func validateContainerDiskPrefixes(prefixes []string) error {
	if err := periapsis.ValidateRunnerImagePrefixes(prefixes); err != nil {
		return fmt.Errorf("containerDisk prefixes: %w", err)
	}
	for _, prefix := range prefixes {
		if !strings.HasSuffix(prefix, "/") || !strings.Contains(prefix, "/") || strings.HasPrefix(prefix, "/") {
			return fmt.Errorf("containerDisk prefix %q must end at a repository path boundary (/)", prefix)
		}
	}
	return nil
}

// ValidateContainerDiskPrefixes checks the administrator's image allowlist.
func ValidateContainerDiskPrefixes(prefixes []string) error {
	return validateContainerDiskPrefixes(prefixes)
}

type vmiUsage struct {
	cores  int
	memory resource.Quantity
}

func vmiCreateUsage(raw string) (vmiUsage, error) {
	converted, err := yaml.YAMLToJSONStrict([]byte(raw))
	if err != nil {
		return vmiUsage{}, err
	}
	var manifest vmiManifest
	if err := json.Unmarshal(converted, &manifest); err != nil {
		return vmiUsage{}, err
	}
	memory, err := resource.ParseQuantity(manifest.Spec.Domain.Resources.Requests.Memory)
	if err != nil {
		return vmiUsage{}, err
	}
	return vmiUsage{cores: manifest.Spec.Domain.CPU.Cores, memory: memory}, nil
}

// KubeVirtConcurrencyGroup serializes a repository's VMI-bearing runs. Each
// run's budget is per run, so without it N concurrent runs of one repository
// could each create a full grant's worth of VMs.
const KubeVirtConcurrencyGroup = "kubevirt"

// DeclaresVMIResources reports whether a workflow has any resource template.
// Admission admits resource templates only as VMI create/get/delete.
func DeclaresVMIResources(workflow *wfv1.Workflow) bool {
	if workflow == nil {
		return false
	}
	for _, template := range workflow.Spec.Templates {
		if template.Resource != nil {
			return true
		}
	}
	return false
}

// RunConcurrencyGroup is the group the scheduler serializes a run on: the
// declared oberth.ci/concurrency-group, or KubeVirtConcurrencyGroup for a
// VMI-bearing workflow that declares none. Admit refuses a VMI-bearing
// workflow that declares any other group.
func RunConcurrencyGroup(workflow *wfv1.Workflow) (string, error) {
	group, err := DeclaredConcurrencyGroup(workflow)
	if err != nil {
		return "", err
	}
	if group == "" && DeclaresVMIResources(workflow) {
		return KubeVirtConcurrencyGroup, nil
	}
	return group, nil
}
