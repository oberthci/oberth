// Copyright 2017-2018 The Argo Authors
// SPDX-License-Identifier: Apache-2.0
//
// Adapted from Argo Workflows v4.0.8 workflow/common/ancestry.go to avoid the
// workflow/common production import. Changes: local names, map-based lookup,
// dependency-name set instead of DependencyType, and fail-closed nil checks.
// See third_party/argo-depends/README.md for provenance and the retained license.
package argoworkflow

import (
	"regexp"
	"sort"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// Keep the upstream token boundaries, including its treatment of trailing dots.
var vmiArgoTaskNameRegex = regexp.MustCompile(`([a-zA-Z0-9][-a-zA-Z0-9]*?\.[A-Z][a-zA-Z]+)|([a-zA-Z0-9][-a-zA-Z0-9]*)`)

func expandVMIArgoDependencies(task *wfv1.DAGTask, tasks map[string]*wfv1.DAGTask) (map[string]bool, string, bool) {
	if task == nil {
		return nil, "", false
	}
	depends := task.Depends
	if depends == "" {
		var legacy []string
		for _, name := range task.Dependencies {
			part, ok := expandVMIArgoDependency(name, tasks[name])
			if !ok {
				return nil, "", false
			}
			legacy = append(legacy, part)
		}
		depends = strings.Join(legacy, " && ")
	}
	matches := vmiArgoTaskNameRegex.FindAllStringSubmatchIndex(depends, -1)
	type expansion struct {
		name       string
		start, end int
	}
	var expansions []expansion
	dependencies := map[string]bool{}
	for _, match := range matches {
		if match[2] != -1 {
			name, _, _ := strings.Cut(depends[match[2]:match[3]], ".")
			if tasks[name] == nil {
				return nil, "", false
			}
			dependencies[name] = true
		} else if match[4] != -1 {
			name := depends[match[4]:match[5]]
			if tasks[name] == nil {
				return nil, "", false
			}
			dependencies[name] = true
			expansions = append(expansions, expansion{name, match[4], match[5]})
		}
	}
	sort.Slice(expansions, func(i, j int) bool { return expansions[i].start > expansions[j].start })
	for _, match := range expansions {
		part, ok := expandVMIArgoDependency(match.name, tasks[match.name])
		if !ok {
			return nil, "", false
		}
		depends = depends[:match.start] + part + depends[match.end:]
	}
	return dependencies, depends, true
}

func expandVMIArgoDependency(name string, task *wfv1.DAGTask) (string, bool) {
	if task == nil {
		return "", false
	}
	statuses := []string{name + ".Succeeded", name + ".Skipped", name + ".Daemoned"}
	if task.ContinueOn != nil {
		if task.ContinueOn.Error {
			statuses = append(statuses, name+".Errored")
		}
		if task.ContinueOn.Failed {
			statuses = append(statuses, name+".Failed")
		}
	}
	return "(" + strings.Join(statuses, " || ") + ")", true
}
