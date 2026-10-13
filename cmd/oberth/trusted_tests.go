package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/oberthci/oberth/internal/vmrunner"
)

// Recipes are public operator policy. Secret-bearing URLs and unpinned images
// are refused; empty configuration keeps trusted execution unavailable.
func readTrustedRecipes(path string) ([]vmrunner.KVMRecipe, error) {
	if path == "" {
		return nil, nil
	}
	// #nosec G304 -- path is an administrator-owned startup flag, never repository input.
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, 128<<10+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 128<<10 {
		return nil, errors.New("trusted recipe file exceeds bounds")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var recipes []vmrunner.KVMRecipe
	if err = dec.Decode(&recipes); err != nil {
		return nil, fmt.Errorf("decode trusted recipes: %w", err)
	}
	var extra any
	if err = dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("trusted recipes require exactly one JSON document")
	}
	if len(recipes) == 0 || len(recipes) > 128 {
		return nil, errors.New("trusted recipes require 1..128 approved recipes")
	}
	seen := map[string]bool{}
	for _, recipe := range recipes {
		if err = recipe.Validate(); err != nil {
			return nil, err
		}
		key := recipe.SuiteSHA256 + recipe.GuestSHA256
		if seen[key] {
			return nil, errors.New("duplicate trusted recipe")
		}
		seen[key] = true
	}
	return recipes, nil
}
