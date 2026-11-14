package model

import "testing"

func TestSecretAccessKeyValidation(t *testing.T) {
	valid := SecretAccessKey{Repo: "upstream/team/repo", Workflow: ".oberth/build.yaml", Step: "camelCase", Path: "oberth/upstream/team/repo/a"}
	for _, step := range []string{"camelCase", "*"} {
		key := valid
		key.Step = step
		if err := key.Validate(); err != nil {
			t.Fatal("valid exact grant refused", err)
		}
	}
	for _, value := range []string{"/other/repo", "upstream/../repo", "upstream//repo", "upstream/./repo", "upstream/team/repo/"} {
		t.Run("repo="+value, func(t *testing.T) {
			key := valid
			key.Repo = value
			if key.Validate() == nil {
				t.Fatal("unclean repository accepted")
			}
		})
	}
	for _, value := range []string{"oberth/upstream/team/repo/*", "oberth/upstream/team/repo/../b", "oberth//a", "/oberth/a", "oberth/a/", `oberth/a" { capabilities = ["read"] }`} {
		t.Run("path="+value, func(t *testing.T) {
			key := valid
			key.Path = value
			if key.Validate() == nil {
				t.Fatal("unclean or widening secret path accepted")
			}
		})
	}
	key := valid
	key.Workflow = "../other.yaml"
	if key.Validate() == nil {
		t.Fatal("escaping workflow accepted")
	}
}
