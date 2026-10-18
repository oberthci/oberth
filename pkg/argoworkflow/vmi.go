package argoworkflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
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

var vmiNamePattern = regexp.MustCompile(`^\{\{workflow\.name\}\}-[a-z0-9]([a-z0-9-]{0,14}[a-z0-9])?$`)
var imageDigestPattern = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)

const (
	maxVMIManifestBytes = 128 << 10
	maxVMICores         = 4
	maxVMIMemory        = "8Gi"
	maxVMIEmptyDisk     = "16Gi"
)

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
		if len(manifest.Spec.Volumes) != 0 || len(manifest.Spec.Networks) != 0 || manifest.Spec.Architecture != "" ||
			manifest.Spec.Domain.CPU.Cores != 0 || manifest.Spec.Domain.Resources.Requests.Memory != "" {
			return fmt.Errorf("argoworkflow: %s.manifest.spec must be absent for %s", where, tmpl.Action)
		}
		return nil
	}
	if len(prefixes) == 0 {
		return fmt.Errorf("argoworkflow: %s has no administrator containerDisk registry allowlist", where)
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
	if len(manifest.Spec.Volumes) == 0 || len(manifest.Spec.Volumes) > 8 {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.volumes must contain 1..8 volumes", where)
	}
	volumeNames := map[string]bool{}
	containerDisks := 0
	for i, volume := range manifest.Spec.Volumes {
		path := fmt.Sprintf("%s.manifest.spec.volumes[%d]", where, i)
		if volume.Name == "" || volumeNames[volume.Name] {
			return fmt.Errorf("argoworkflow: %s.name must be unique and nonempty", path)
		}
		volumeNames[volume.Name] = true
		kinds := 0
		if volume.ContainerDisk != nil {
			kinds++
			containerDisks++
			image := volume.ContainerDisk.Image
			allowed := false
			for _, prefix := range prefixes {
				if prefix != "" && strings.HasPrefix(image, prefix) {
					allowed = true
				}
			}
			if !allowed || !imageDigestPattern.MatchString(image) {
				return fmt.Errorf("argoworkflow: %s.containerDisk.image must have an allowlisted prefix and sha256 digest", path)
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
		if !volumeNames[disk.Name] || seenDisks[disk.Name] || disk.Disk.Bus != "virtio" {
			return fmt.Errorf("argoworkflow: %s.manifest.spec.domain.devices.disks[%d] must name a unique volume with virtio bus", where, i)
		}
		seenDisks[disk.Name] = true
	}
	if len(manifest.Spec.Networks) != 1 || len(manifest.Spec.Domain.Devices.Interfaces) != 1 ||
		manifest.Spec.Networks[0].Name == "" || manifest.Spec.Networks[0].Name != manifest.Spec.Domain.Devices.Interfaces[0].Name {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.networks and domain.devices.interfaces must define one matching pod/masquerade network", where)
	}
	if manifest.Spec.Networks[0].Pod == nil || manifest.Spec.Domain.Devices.Interfaces[0].Masquerade == nil {
		return fmt.Errorf("argoworkflow: %s.manifest.spec.networks and domain.devices.interfaces require pod and masquerade", where)
	}
	return nil
}
