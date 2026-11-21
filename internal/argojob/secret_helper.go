package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/pkg/argoworkflow/defaults"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const secretHelperDirectory = "/run/oberth/bin"

// SecretHelperVolumeBytes bounds the supplied program on disk; the built
// program stays at most half of it.
const SecretHelperVolumeBytes = 256 << 20

// The installation image and copy command are sealed with the run. Only leaves
// whose own template declares secret paths, and so hold grants, receive the program.
func appendSecretHelper(leaf nativeLeaf, image string, declared bool) error {
	if !declared {
		return nil
	}
	t, err := defaults.Template(leaf.workflow.Spec.TemplateDefaults, leaf.template)
	if err != nil {
		return err
	}
	if len(secretContainers(t)) == 0 {
		return nil
	}
	if _, err := name.NewDigest(image); err != nil {
		return publicerr.New(publicerr.InvalidInput, helperImageReason, helperImageRemedy, nil)
	}
	mount := core.VolumeMount{Name: "oberth-secret-bin", MountPath: secretHelperDirectory, ReadOnly: true}
	for _, c := range secretContainers(t) {
		c.VolumeMounts = append(c.VolumeMounts, mount)
		if len(c.Command) > 0 && c.Command[0] == "oberth" {
			c.Command[0] = secretHelperDirectory + "/oberth"
		}
	}
	t.Volumes = append(t.Volumes, core.Volume{Name: mount.Name, VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(SecretHelperVolumeBytes, resource.BinarySI)}}})
	mount.ReadOnly = false
	installer := wf.UserContainer{Container: core.Container{
		Name: "oberth-secret-install", Image: image, ImagePullPolicy: core.PullIfNotPresent,
		Command:      []string{"/usr/local/bin/oberth", "internal-install", "--to", secretHelperDirectory + "/oberth"},
		VolumeMounts: []core.VolumeMount{mount},
		SecurityContext: &core.SecurityContext{
			RunAsUser: new(int64(65532)), RunAsNonRoot: new(true), ReadOnlyRootFilesystem: new(true),
			AllowPrivilegeEscalation: new(false), Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}},
			SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
		},
	}, MirrorVolumeMounts: new(false)}
	t.InitContainers = append([]wf.UserContainer{installer}, t.InitContainers...)
	*leaf.template = *t
	return nil
}
