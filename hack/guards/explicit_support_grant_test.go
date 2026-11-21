package main

import "testing"

func TestExplicitGrantPermitsSupportWithoutGrantingItsParent(t *testing.T) {
	p := fixture(t)
	p.Allow["C1"] = nil
	p.TestAllow["component/a"] = []string{"component/b/gittest"}
	p.Rules = []rule{{ID: 6, From: []string{"@C1"}, To: []string{"@C2"}, Effect: "deny"}}
	if p.permitted("component/a", "component/b", true) {
		t.Fatal("parent runtime package was granted")
	}
	files := map[string]string{
		"component/a/use_test.go":        "package a\nimport _ \"example.invalid/unit/component/b/gittest\"\n",
		"component/b/value.go":           "package b\n",
		"component/b/gittest/fixture.go": "package gittest\n",
	}
	clean(t, scan(t, p, files))
}
