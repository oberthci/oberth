package identityname

import "testing"

func TestIdentityNamesMatchFixedTrustedNames(t *testing.T) {
	cases := []struct {
		name string
		key  Key
		kind Kind
		want string
	}{
		{"repository", Key{Repository: "github/oberthci/oberth", File: "", Step: ""}, Pipeline, "oberth-argo-github-oberthci-oberth-2b5c2901d41d1d11d1f9"},
		{"repository_ci", Key{Repository: "github/oberthci/oberth", File: "", Step: ""}, PipelineCI, "oberth-argo-ci-github-oberthci-oberth-2b5c2901d41d1d11d1f9"},
		{"images", Key{Repository: "github/oberthci/oberth", File: ".oberth/release.yaml", Step: "release-publish-images"}, Step, "oberth-step-github-oberthci-oberth--oberth-a78e9e7b9fb5f6ec80d9"},
		{"chart", Key{Repository: "github/oberthci/oberth", File: ".oberth/release.yaml", Step: "release-publish-chart"}, Step, "oberth-step-github-oberthci-oberth--oberth-c013db1cb6134b2e9e4e"},
		{"images_ci", Key{Repository: "github/oberthci/oberth", File: ".oberth/release.yaml", Step: "release-publish-images"}, StepCI, "oberth-step-ci-github-oberthci-oberth--obe-a78e9e7b9fb5f6ec80d9"},
		{"sanitized", Key{Repository: "forge/example.org/my_repo", File: ".oberth/release.yaml", Step: "publish"}, Step, "oberth-step-forge-example-org-my-repo--obe-3867e0b11c600b1f353e"},
		{"long", Key{Repository: "forge/organization/long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-repo", File: ".oberth/release.yaml", Step: "publish"}, Step, "oberth-step-forge-organization-long-long-l-fb98fa0834655e3e48e0"},
		{"long_ci", Key{Repository: "forge/organization/long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-long-repo", File: ".oberth/release.yaml", Step: "publish"}, StepCI, "oberth-step-ci-forge-organization-long-lon-fb98fa0834655e3e48e0"},
		{"repository_fallback", Key{Repository: "---/---/---", File: "", Step: ""}, Pipeline, "oberth-argo-repo-29df7bd3d510bdbf5b06"},
		{"repository_ci_fallback", Key{Repository: "---/---/---", File: "", Step: ""}, PipelineCI, "oberth-argo-ci-repo-29df7bd3d510bdbf5b06"},
		{"step_fallback", Key{Repository: "---/---/---", File: "---", Step: "---"}, Step, "oberth-step-step-2139b1cb7c9477078b77"},
		{"step_ci_fallback", Key{Repository: "---/---/---", File: "---", Step: "---"}, StepCI, "oberth-step-ci-step-2139b1cb7c9477078b77"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Name(tc.key, tc.kind)
			if err != nil || got != tc.want {
				t.Fatalf("name = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
