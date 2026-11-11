package model

import "testing"

func TestReleaseVersionTagBoundary(t *testing.T) {
	for _, tag := range []string{"v1.2.3", "v01.2.3", "v1.2.3-rc.1", "v1.2.3+build", "v1.2.3-extra"} {
		if !IsReleaseVersionTag(tag) {
			t.Fatalf("release name not reserved: %s", tag)
		}
	}
	for _, tag := range []string{"deploy-1", "test/1", "v1", "v1.2", "1.2.3", "refs/tags/v1.2.3"} {
		if IsReleaseVersionTag(tag) {
			t.Fatalf("ordinary name classified as release: %s", tag)
		}
	}
}
