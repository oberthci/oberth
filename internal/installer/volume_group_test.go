package installer_test

import (
	core "k8s.io/api/core/v1"
	"testing"
)

func TestServerVolumePreservesPrivateFilesAcrossPodReplacement(t *testing.T) {
	t.Parallel()
	s := serverDeployment(t, rendered(t, nil)).Spec.Template.Spec.SecurityContext
	if s.FSGroup == nil || *s.FSGroup != 65534 || s.RunAsGroup == nil || *s.RunAsGroup != *s.FSGroup {
		t.Fatal("server volume group differs from process group")
	}
	if s.FSGroupChangePolicy == nil || *s.FSGroupChangePolicy != core.FSGroupChangeOnRootMismatch {
		t.Fatal("replacement Pod may recursively broaden private file permissions")
	}
}
