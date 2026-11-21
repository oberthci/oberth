package main

import (
	"path"
	"slices"
	"strings"
)

type rule struct {
	ID       int
	From, To []string
	Effect   string
	Symbols  map[string][]string
}
type policy struct {
	Version            int
	ProductionCommands []string `json:"production_commands"`
	ProductCommand     string   `json:"product_command"`
	Components, Allow  map[string][]string
	Shared, Retired    []string
	Rules              []rule
	PackageAllow       map[string][]string `json:"package_allow"`
	SharedDependencies map[string][]string `json:"shared_dependencies"`
	TestAllow          map[string][]string `json:"test_allow"`
	Packages           map[string]struct {
		Owner, Kind string
		Consumers   []string
	}
	Harness           struct{ Trees []string }
	production, mains map[string]bool
	FixtureOnly       []struct{ From, To []string } `json:"fixture_only"`
}

func (p policy) matches(value string, patterns []string) bool {
	return slices.ContainsFunc(patterns, func(pattern string) bool {
		if strings.HasPrefix(pattern, "@") {
			return p.reference(value, pattern[1:])
		}
		base := strings.TrimSuffix(pattern, "/**")
		return pattern == "**" || value == pattern || (base != pattern && (value == base || strings.HasPrefix(value, base+"/")))
	})
}
func (p policy) reference(value, ref string) bool {
	if ref != "shared" && p.harness(value) != "" {
		return false
	}
	if ref == "shared" {
		return p.matches(value, p.Shared)
	}
	if strings.HasPrefix(ref, "components") {
		except := strings.TrimPrefix(ref, "components:except:")
		for owner, paths := range p.Components {
			if owner != except && p.matches(value, paths) {
				return true
			}
		}
	}
	return p.matches(value, p.Components[ref])
}
func (p policy) owner(value string) string {
	owners := map[string]bool{p.Packages[value].Owner: true}
	delete(owners, "")
	if p.harness(value) != "" {
		return "harness"
	}
	if parent := p.supportParent(value); parent != "" {
		return p.owner(parent)
	}
	for owner, patterns := range p.Components {
		if p.matches(value, patterns) {
			owners[owner] = true
		}
	}
	for _, fixture := range p.FixtureOnly {
		if p.matches(value, fixture.From) {
			owners["fixture"] = true
		}
	}
	if len(owners) == 1 {
		for owner := range owners {
			return owner
		}
	}
	return ""
}
func (p policy) permitted(from, to string, test bool) bool {
	test = test || p.Packages[from].Kind == "test_only"
	if test && p.matches(to, p.TestAllow[from]) {
		return true
	}
	for _, fixture := range p.FixtureOnly {
		if p.matches(from, fixture.From) && p.matches(to, fixture.To) {
			return true
		}
	}
	if allowed, handled := p.toolPermission(from, to, test); handled {
		return allowed
	}
	if p.matches(from, p.Shared) {
		return p.matches(to, p.SharedDependencies[from])
	}
	if override, ok := p.PackageAllow[from]; ok {
		return p.matches(to, override)
	}
	return p.matches(to, p.Allow[p.owner(from)])
}
func (p policy) harness(value string) string {
	for _, tree := range p.Harness.Trees {
		if p.matches(value, []string{tree}) {
			return tree
		}
	}
	return ""
}
func (p policy) supportParent(value string) string {
	name, parent := path.Base(value), path.Dir(value)
	if parent != value && !p.mains[value] && (name == "fake" || name == "contracttest" || strings.HasSuffix(name, "test") && !p.production[value]) && p.owner(parent) != "" {
		return parent
	}
	return ""
}
func (p policy) toolPermission(from, to string, test bool) (bool, bool) {
	if tree := p.harness(from); tree != "" {
		return tree == p.harness(to) || p.matches(to, p.TestAllow[from]), true
	}
	if p.harness(to) != "" {
		return false, true
	}
	if parent := p.supportParent(from); parent != "" {
		return parent == to || p.permitted(parent, to, true), true
	}
	if parent := p.supportParent(to); parent != "" {
		return test && (from == parent || p.permitted(from, parent, true)), true
	}
	kit := p.Packages[to]
	return test && (p.matches(to, p.TestAllow[from]) || slices.Contains(kit.Consumers, p.owner(from))), kit.Kind == "test_only"
}
