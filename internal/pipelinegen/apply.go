package pipelinegen

import (
	"fmt"
	"strings"
)

// Apply folds what the Actions workflow says into what the checkout says.
//
// The workflow wins on versions and flags, because that is the configuration
// the repository actually builds green with today. The checkout wins on
// commands, because a workflow that delegates to a reusable workflow states no
// commands at all.
func Apply(workflow Workflow, project *Project) {
	job, ok := buildJob(workflow)
	if !ok {
		project.cannot("no build job was found in " + shortPath(workflow.Path) + "; the steps below come from this repository's own manifests")
		return
	}
	project.note("GitHub Actions: " + shortPath(workflow.Path) + " job " + job.ID)

	if version := majorVersion(job.With["NODEJS_VERSION"]); version != "" {
		project.NodeMajor = version
		project.note("Actions input NODEJS_VERSION: " + version)
	}
	if version := majorVersion(job.With["JAVA_VERSION"]); version != "" {
		project.JavaMajor = version
		project.note("Actions input JAVA_VERSION: " + version)
	}
	if strings.EqualFold(job.With["LEGACY_PEER_DEPS"], "true") {
		project.LegacyPeerDeps = true
		project.note("Actions input LEGACY_PEER_DEPS: npm installs with --legacy-peer-deps")
	}

	if job.Uses != "" {
		project.cannot(fmt.Sprintf(
			"the build is delegated to %s, which is not in this repository, so its steps could not be read; the steps below are this repository's own build and test commands",
			job.Uses))
	}
	if len(job.Secret) > 0 {
		project.cannot("Actions passed these secrets to the build and Oberth has none of them: " +
			strings.Join(job.Secret, ", "))
	}

	for _, step := range job.Steps {
		if strings.TrimSpace(step.Run) != "" {
			continue
		}
		if step.Uses != "" && !knownSetupAction(step.Uses) {
			project.cannot("Actions step " + describeStep(step) + " uses " + step.Uses + ", which has no equivalent here")
		}
	}
}

// InlineRunSteps returns the shell steps an Actions job declares directly.
// These are the only steps that can be translated literally; everything else
// is an action whose behaviour lives elsewhere.
func InlineRunSteps(workflow Workflow) []Step {
	job, ok := buildJob(workflow)
	if !ok {
		return nil
	}
	var runs []Step
	for _, step := range job.Steps {
		if strings.TrimSpace(step.Run) != "" {
			runs = append(runs, step)
		}
	}
	return runs
}

// buildJob picks the job that builds. A single-job workflow is that job; a
// multi-job workflow is searched for one named build, then test or ci.
func buildJob(workflow Workflow) (Job, bool) {
	if len(workflow.Jobs) == 0 {
		return Job{}, false
	}
	if len(workflow.Jobs) == 1 {
		return workflow.Jobs[0], true
	}
	for _, wanted := range []string{"build", "test", "ci"} {
		for _, job := range workflow.Jobs {
			if strings.EqualFold(job.ID, wanted) || strings.EqualFold(job.Name, wanted) {
				return job, true
			}
		}
	}
	return Job{}, false
}

// knownSetupAction lists the actions whose whole effect is already covered by
// running in a toolchain image with the checkout mounted. Naming them keeps
// the untranslated list to the steps that genuinely do something else.
func knownSetupAction(uses string) bool {
	for _, prefix := range []string{
		"actions/checkout", "actions/setup-node", "actions/setup-java",
		"actions/setup-go", "actions/cache",
	} {
		if strings.HasPrefix(uses, prefix) {
			return true
		}
	}
	return false
}

func describeStep(step Step) string {
	if strings.TrimSpace(step.Name) != "" {
		return `"` + step.Name + `"`
	}
	return "(unnamed)"
}

func shortPath(path string) string {
	if index := strings.Index(path, ".github/"); index >= 0 {
		return path[index:]
	}
	return path
}
