package workflowpolicy

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/identityname"
)

func grantFixture() GrantKey {
	return GrantKey{Key: identityname.Key{Repository: "forge/org/repo", File: BuildFile, Step: "main"}, Path: "public/signing"}
}

func TestSecretAndIdentityGrantsShareTheTuple(t *testing.T) {
	key := grantFixture()
	for _, audience := range []string{"", "custom-audience"} {
		key.Audience = audience
		if err := key.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []string{"*", "[0].publish", "main[0].publish", "main.task(0:item)", "main.tasks[1].check", "[0].publish(0:hello world)", "publish(0:*.txt)", "publish(0:a?b)"} {
		key.Step = step
		if err := key.Validate(); err != nil {
			t.Fatal(step, err)
		}
	}
	raw, err := json.Marshal(grantFixture())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"repo":"forge/org/repo","file":".oberth/build.yaml","step":"main","path":"public/signing","audience":""}` {
		t.Fatalf("tuple is not flat: %s", raw)
	}
}

func TestGrantRefusalsAreIndependent(t *testing.T) {
	cases := []struct {
		name   string
		change func(*GrantKey)
	}{
		{"missing_repo", func(k *GrantKey) { k.Repository = "" }}, {"invalid_repo", func(k *GrantKey) { k.Repository = "bare" }},
		{"missing_file", func(k *GrantKey) { k.File = "" }}, {"wildcard_file", func(k *GrantKey) { k.File = "*.yaml" }},
		{"missing_step", func(k *GrantKey) { k.Step = "" }}, {"placeholder_step", func(k *GrantKey) { k.Step = "?" }},
		{"nul_step", func(k *GrantKey) { k.Step = "a\x00b" }}, {"cr_step", func(k *GrantKey) { k.Step = "a\rb" }},
		{"lf_step", func(k *GrantKey) { k.Step = "a\nb" }}, {"missing_path", func(k *GrantKey) { k.Path = "" }},
		{"wildcard_path", func(k *GrantKey) { k.Path = "public/*" }}, {"traversal_path", func(k *GrantKey) { k.Path = "public/../signing" }},
		{"leading_parent_path", func(k *GrantKey) { k.Path = "../signing" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { key := grantFixture(); tc.change(&key); requireInvalid(t, key.Validate()) })
	}
	for _, audience := range []string{"aud\nother", "aud other", "aud\x00other", "aud\x01other", "aud\tother", "aud\rother", "aud\u2003other"} {
		key := grantFixture()
		key.Audience = audience
		requireInvalid(t, key.Validate())
	}
}

func TestWildcardGrantMatchesOnlyItsRepositoryAndFile(t *testing.T) {
	grant := grantFixture()
	grant.Step = "*"
	for _, step := range []string{"main", "other", "main[0].publish"} {
		key := grant.Key
		key.Step = step
		if !grant.Matches(key) {
			t.Fatal("wildcard does not cover file step", step)
		}
	}
	for _, key := range []identityname.Key{{}, {Repository: grant.Repository, File: grant.File},
		{Repository: "forge/org/other", File: grant.File, Step: "main"},
		{Repository: grant.Repository, File: ReleaseFile, Step: "main"}} {
		if grant.Matches(key) {
			t.Fatal("wildcard escaped its exact file", key)
		}
	}
	grant.Step = "main"
	if !grant.Matches(grant.Key) {
		t.Fatal("exact step does not match")
	}
	key := grant.Key
	key.Step = "other"
	if grant.Matches(key) {
		t.Fatal("named grant matched another step")
	}
	if (GrantKey{}).Matches(identityname.Key{}) {
		t.Fatal("zero grant matches")
	}
}
