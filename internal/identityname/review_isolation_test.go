package identityname

import "testing"

func TestReviewIdentityIsolation(t *testing.T) {
	t.Helper()
	base := Key{Repository: "forge/org/repo", File: ".oberth/release.yaml", Step: "publish"}
	seen := map[string]bool{}
	for _, kind := range []Kind{Step, StepCI} {
		keys := []Key{base}
		for _, r := range []string{"forge/org/repository", "forge/org/Repo", "forge/org/repо"} {
			k := base
			k.Repository = r
			keys = append(keys, k)
		}
		for _, file := range []string{".oberth/release.yaml2", ".oberth/Release.yaml", ".oberth/../release.yaml"} {
			k := base
			k.File = file
			keys = append(keys, k)
		}
		for _, step := range []string{"publish2", "Publish", "publіsh", "*"} {
			k := base
			k.Step = step
			keys = append(keys, k)
		}
		for _, key := range keys {
			name, err := Name(key, kind)
			if err != nil {
				t.Fatal(err)
			}
			if seen[name] {
				t.Fatal("repository/file/step or CI kind aliases another grant")
			}
			seen[name] = true
		}
	}

}
