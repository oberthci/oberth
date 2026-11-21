package workflowpolicy

import "testing"

func TestReleaseVersionRequiresStableThreePartVersion(t *testing.T) {
	for _, v := range []string{"v0.0.0", "v1.2.3", "v999.0.1"} {
		if !IsReleaseVersion(v) {
			t.Fatal("stable version refused", v)
		}
	}
	for _, v := range []string{"", "v1", "v1.2", "v01.2.3", "v1.02.3", "v1.2.03", "v1.2.3-rc.1", "v1.2.3+build", "v1.2.3junk", "v1.2.3/x", "v1.2.3.4", "refs/tags/v1.2.3"} {
		if IsReleaseVersion(v) {
			t.Fatal("non-release version accepted", v)
		}
	}
}
