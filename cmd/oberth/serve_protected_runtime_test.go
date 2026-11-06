package main

import (
	"io"
	"testing"
)

func TestServeProtectedRuntimeRequiresExplicitOptIn(t *testing.T) {
	options, err := parseServeOptions([]string{"--argo-namespace", "pipeline"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.trustedRuntime != "legacy" {
		t.Fatal("default changed protected runtime")
	}
	for _, mode := range []string{"legacy", "generic-capability", "invalid"} {
		for _, recipes := range []string{"", "/operator/recipes.json"} {
			t.Run(mode+recipes, func(t *testing.T) {
				options := minimalValidServeOptions()
				options.trustedRuntime, options.trustedRecipesFile = mode, recipes
				err := validateServeOptions(options)
				wantValid := mode == "legacy" || mode == "generic-capability" && recipes != ""
				if (err == nil) != wantValid {
					t.Fatalf("mode=%s recipes=%s err=%v", mode, recipes, err)
				}
			})
		}
	}
}
