package releaseverify

import (
	"net/url"
	"testing"
)

func TestRedirectAllowedOnlyOverHTTPSToReleaseHosts(t *testing.T) {
	for _, tc := range []struct {
		target  string
		allowed bool
	}{
		{"https://release-assets.githubusercontent.com/github-production-release-asset/1/2?sig=x", true},
		{"https://objects.githubusercontent.com/github-production-release-asset-2e65be/1/2", true},
		{Origin + "/v1.2.3/release.json", true},
		{"http://release-assets.githubusercontent.com/asset", false},
		{"https://release-assets.githubusercontent.com.example.test/asset", false},
		{"https://release-assets.githubusercontent.com:8443/asset", false},
		{"https://user@release-assets.githubusercontent.com/asset", false},
		{"https://example.test/asset", false},
		{"https://github.com/other/project/releases/download/v1.2.3/release.json", false},
		{Origin + "/../../../other/project/releases/download/v1.2.3/release.json", false},
		{"https://github.com/oberthci/oberth/releases/download", false},
		{"ftp://release-assets.githubusercontent.com/asset", false},
	} {
		t.Run(tc.target, func(t *testing.T) {
			target, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if RedirectAllowed(target) != tc.allowed {
				t.Fatal("redirect decision differs", tc.allowed)
			}
		})
	}
}
