package pipelinegen

import (
	"fmt"
	"strings"
)

// step is one container template plus the sequential position it occupies.
type step struct {
	name    string
	comment string
	image   string
	// script is a /bin/sh -c body. It always begins with `set -eu`, so a
	// failing command inside a step fails the step.
	script string
	// secret makes the step run under `oberth secretstore exec`, which fetches
	// the declared path and writes it into the server-owned tmpfs.
	secret bool
}

// Result is a generated document and what the caller should tell the operator.
type Result struct {
	// YAML is the .oberth/build.yaml content.
	YAML string
	// Complete is false when the generator could not produce a pipeline that
	// really builds the repository. The document is then a scaffold whose
	// steps fail until a human finishes them.
	Complete bool
	// SecretPath is the declared secret-store path, empty when the pipeline
	// needs no credential. A declared path needs a matching grant before the
	// first push, and GrantCommand is exactly that command.
	SecretPath   string
	GrantCommand string
	// Steps names the steps in order, for the summary line.
	Steps []string
}

const (
	sourceMount  = "/work/src"
	buildDir     = "/work/build"
	secretsDir   = "/run/oberth-secrets"
	oberthBinary = "/run/oberth/bin/oberth"
)

// Generate turns a detected project into a pipeline document.
//
// The document is a single flat Workflow with a SEQUENTIAL step chain, not a
// DAG. Argo lets the siblings of a failed DAG task run to completion, so a red
// lint keeps a five-minute test suite running and the run reports minutes
// after it already knew the answer. Chained steps stop at the first red.
func Generate(project Project) Result {
	steps, complete := planSteps(project)

	secretPath := ""
	if project.PrivateRegistry && project.Org != "" && usesSecret(steps) {
		secretPath = "oberth/upstream/" + project.Org + "/github-token"
	}

	result := Result{Complete: complete, SecretPath: secretPath}
	for _, one := range steps {
		result.Steps = append(result.Steps, one.name)
	}
	if secretPath != "" {
		name := project.Repo
		if name == "" {
			name = "<repo>"
		}
		result.GrantCommand = fmt.Sprintf("oberth access allow %s '*' %s", name, secretPath)
	}
	result.YAML = render(project, steps, result)
	return result
}

func usesSecret(steps []step) bool {
	for _, one := range steps {
		if one.secret {
			return true
		}
	}
	return false
}

// planSteps chooses the chain for a project kind.
func planSteps(project Project) (steps []step, complete bool) {
	switch project.Kind {
	case KindNode:
		return nodeSteps(project), true
	case KindMaven:
		return mavenSteps(project), true
	case KindGo:
		return goSteps(project), true
	default:
		return scaffoldSteps(), false
	}
}

// copySource is the first step of every chain.
//
// /work/src is the checkout and it is READ-ONLY: the server mounts a per-run
// claim it filled with this revision, so nothing may write there. `npm ci`
// creates node_modules in the working directory and fails with ENOENT against
// a read-only mount, and Maven writes target/ in the same place. Copying once
// costs a few seconds and makes every later step ordinary.
func copySource(image string) step {
	return step{
		name:    "copy-source",
		comment: "/work/src is the read-only checkout; every later step builds in a writable copy.",
		image:   image,
		script: strings.Join([]string{
			"set -eu",
			"mkdir -p " + buildDir,
			"cp -a " + sourceMount + "/. " + buildDir + "/",
			"cd " + buildDir,
			`echo "copied $(find . -type f | wc -l) files"`,
		}, "\n"),
	}
}

func nodeSteps(project Project) []step {
	image, _ := nodeImage(project.NodeMajor)
	steps := []step{copySource(image)}

	install := "npm ci --no-audit --no-fund"
	if project.LegacyPeerDeps {
		install += " --legacy-peer-deps"
	}
	steps = append(steps, step{
		name:    "install",
		comment: "npm ci, in the copy. A private scope needs the upstream token, which arrives as a file.",
		image:   image,
		secret:  project.PrivateRegistry && project.Org != "",
		script:  "set -eu\ncd " + buildDir + "\n" + install,
	})

	// The repository's own scripts, in the order a build runs them. Only
	// scripts that exist are emitted: inventing `npm run lint` for a
	// repository with no lint script produces a step that fails for a reason
	// that has nothing to do with the code.
	for _, candidate := range []struct{ script, comment string }{
		{"lint", "the repository's own lint script"},
		{"typecheck", "the repository's own typecheck script. Invoking the compiler through npx is deliberately avoided: with no local typescript, npx fetches the long-abandoned standalone `tsc` package, which reports TS6046 about --jsx and reads like a config error"},
		{"test", "the repository's own test script; CI=true makes a watch-mode runner run once and exit"},
		{"build", "the repository's own build script"},
	} {
		if _, ok := project.script(candidate.script); !ok {
			continue
		}
		steps = append(steps, step{
			name:    candidate.script,
			comment: candidate.comment,
			image:   image,
			script:  "set -eu\ncd " + buildDir + "\nnpm run " + candidate.script + " --if-present",
		})
	}
	return steps
}

func mavenSteps(project Project) []step {
	image, _ := mavenImage(project.JavaMajor)
	credentialed := project.PrivateRegistry && project.Org != ""

	settings := ""
	if credentialed {
		// The server id must match the <repository><id> the pom (or its
		// parent) declares. `github` is the convention for GitHub Packages;
		// when the pom uses another id this file has to say that id instead.
		settings = strings.Join([]string{
			"mkdir -p " + buildDir + "/.oberth-m2",
			"cat > " + buildDir + "/.oberth-m2/settings.xml <<'XML'",
			`<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0">`,
			"  <servers>",
			"    <server>",
			"      <id>github</id>",
			"      <username>x-access-token</username>",
			"      <password>${env.OBERTH_UPSTREAM_TOKEN}</password>",
			"    </server>",
			"  </servers>",
			"</settings>",
			"XML",
		}, "\n") + "\n"
	}

	mavenFlags := "-B -ntp -Dmaven.repo.local=/work/cache/m2"
	if credentialed {
		mavenFlags += " -s " + buildDir + "/.oberth-m2/settings.xml"
	}

	return []step{
		copySource(image),
		{
			name:    "test",
			comment: "mvn test, with the local repository on the run cache so a second run does not refetch the world",
			image:   image,
			secret:  credentialed,
			script:  "set -eu\ncd " + buildDir + "\n" + settings + "mvn " + mavenFlags + " test",
		},
		{
			name:    "package",
			comment: "package without re-running the tests the previous step already ran",
			image:   image,
			secret:  credentialed,
			script:  "set -eu\ncd " + buildDir + "\n" + settings + "mvn " + mavenFlags + " -DskipTests package",
		},
	}
}

func goSteps(Project) []step {
	return []step{
		copySource(imageGo),
		{name: "vet", image: imageGo, comment: "go vet", script: "set -eu\ncd " + buildDir + "\ngo vet ./..."},
		{name: "test", image: imageGo, comment: "go test", script: "set -eu\ncd " + buildDir + "\ngo test ./..."},
		{name: "build", image: imageGo, comment: "go build", script: "set -eu\ncd " + buildDir + "\ngo build ./..."},
	}
}

// scaffoldSteps is what an unrecognized repository gets.
//
// It fails on purpose. The generator this replaces emitted a demo that copied
// a file and checksummed it, which went green on every repository and so told
// nobody that nothing had been translated. A red first run with an error that
// names the file to edit is the honest version of "I could not do this".
func scaffoldSteps() []step {
	return []step{{
		name:    "unfinished",
		comment: "THIS PIPELINE IS NOT FINISHED. Replace this step with the repository's real build.",
		image:   imageDebian,
		script: strings.Join([]string{
			"set -eu",
			`echo "oberth init could not tell how this repository is built." >&2`,
			`echo "Edit .oberth/build.yaml and replace the 'unfinished' step with the real" >&2`,
			`echo "build and test commands, then push again." >&2`,
			"exit 1",
		}, "\n"),
	}}
}
