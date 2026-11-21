package main

import "testing"

func TestExplicitTestGrantIsTheOnlyExceptionToADenyRule(t *testing.T) {
	p := fixture(t)
	p.Rules = []rule{{ID: 2, From: []string{"@C2"}, To: []string{"@C1"}, Effect: "deny"}}
	body := "package b\nimport alias \"example.invalid/unit/component/a\"\nvar _=alias.Value\n"
	require(t, scan(t, p, map[string]string{"component/b/a_test.go": body}), "DEP02")
	p.TestAllow["component/b"] = []string{"component/a"}
	clean(t, scan(t, p, map[string]string{"component/b/a_test.go": body}))
	require(t, scan(t, p, map[string]string{"component/b/prod.go": body}), "DEP02")
}
