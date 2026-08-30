package pipelinegen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind is what the repository builds with. It is deliberately coarse: the
// generator only needs to know which toolchain image to run and which command
// installs dependencies.
type Kind string

const (
	KindNode    Kind = "node"
	KindMaven   Kind = "maven"
	KindGo      Kind = "go"
	KindUnknown Kind = "unknown"
)

// Project is everything the generator learned about a checkout.
//
// Every field records where it came from, because the generated header prints
// the provenance. A user who can see that the Node major came from .nvmrc and
// the test command came from package.json can check both in seconds; a user
// handed an unattributed pipeline has to read all of it.
type Project struct {
	Kind Kind

	// NodeMajor / JavaMajor are the major versions the repository asked for,
	// from the Actions workflow inputs or from a version file.
	NodeMajor string
	JavaMajor string

	// Scripts are the package.json scripts, by name.
	Scripts map[string]string

	// PackageManager is npm, pnpm or yarn. The lockfile decides, because it
	// is the file that has to be honoured: running `npm ci` in a repository
	// whose lockfile is pnpm's either fails outright or silently resolves a
	// different dependency graph than every developer has.
	//
	// PackageManagerMajor is the major that lockfile format belongs to, so
	// the pipeline can name a version instead of taking whatever the registry
	// calls latest on the day it runs. Empty when nothing said which.
	PackageManager      string
	PackageManagerMajor string

	// Lockfile records that one was found at all. Without one there is
	// nothing to reproduce, and `npm ci` refuses to run.
	Lockfile bool

	// Workspaces reports a pnpm workspace root (pnpm-workspace.yaml). It
	// changes what a script name means: a root script is one package's, and
	// the suite usually lives in the members.
	Workspaces bool

	// LegacyPeerDeps mirrors the Actions input of the same name: npm 7+
	// refuses a peer-dependency conflict that npm 6 accepted, and a repository
	// that built on Actions with the flag will not install without it.
	LegacyPeerDeps bool

	// PrivateRegistry reports that installing dependencies needs a credential,
	// and Registry names the host that wants it.
	PrivateRegistry bool
	Registry        string

	// Org is the upstream organization, which is what scopes the secret the
	// private registry needs. Repo is the repository name Oberth catalogs it
	// under, which is what a grant is keyed by.
	//
	// Org is NOT detected. It is the trailing segment of the base URL an
	// upstream was registered with on the server, so only the server knows it;
	// a checkout cloned from a local path or a mirror yields a different word,
	// and the secret path built from that word is refused at admission. The
	// caller supplies it.
	Org  string
	Repo string

	// OriginOrg is what the origin remote suggests the org is. It is carried
	// for one purpose: saying so in the generated file when it disagrees with
	// the registered org. It is never used to build a path.
	OriginOrg string

	// Provenance and honesty.
	Sources      []string
	Untranslated []string
}

// note records a provenance line.
func (p *Project) note(line string) { p.Sources = append(p.Sources, line) }

// cannot records something the generator saw and did not translate. A step
// that exists on Actions and not here has to be visible, or the pipeline
// quietly tests less than the repository thinks it does.
func (p *Project) cannot(line string) { p.Untranslated = append(p.Untranslated, line) }

// script returns a package.json script body and whether it exists.
func (p Project) script(name string) (string, bool) {
	body, ok := p.Scripts[name]
	return strings.TrimSpace(body), ok && strings.TrimSpace(body) != ""
}

// DetectProject reads the checkout at root. It never fails: an unreadable or
// absent file means one less thing known, and the caller turns "nothing known"
// into a scaffold that says so.
func DetectProject(root string) Project {
	project := Project{Kind: KindUnknown, Scripts: map[string]string{}}

	if raw, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		var manifest struct {
			Scripts map[string]string `json:"scripts"`
			Engines struct {
				Node string `json:"node"`
			} `json:"engines"`
			// packageManager is the authoritative statement when it exists:
			// the repository names the tool and the exact version it expects,
			// and a lockfile format can only imply a range.
			PackageManager string `json:"packageManager"`
		}
		if json.Unmarshal(raw, &manifest) == nil {
			project.Kind = KindNode
			project.Scripts = manifest.Scripts
			if names := scriptNames(manifest.Scripts); len(names) > 0 {
				project.note("package.json scripts: " + strings.Join(names, ", "))
			} else {
				project.note("package.json found, with no scripts")
			}
			if major := majorVersion(manifest.Engines.Node); major != "" {
				project.NodeMajor = major
				project.note("package.json engines.node: " + major)
			}
			if name, major := splitPackageManagerField(manifest.PackageManager); name != "" {
				project.PackageManager = name
				project.PackageManagerMajor = major
				project.note("package.json packageManager: " + strings.TrimSpace(manifest.PackageManager))
			}
		}
	}

	detectPackageManager(root, &project)

	if raw, err := os.ReadFile(filepath.Join(root, ".nvmrc")); err == nil {
		if major := majorVersion(string(raw)); major != "" {
			project.NodeMajor = major
			project.note(".nvmrc: Node " + major)
		}
	}

	if raw, err := os.ReadFile(filepath.Join(root, ".npmrc")); err == nil {
		if host := authenticatedRegistryHost(string(raw)); host != "" {
			project.PrivateRegistry = true
			project.Registry = host
			project.note(".npmrc: authenticated registry " + host)
		}
	}

	if raw, err := os.ReadFile(filepath.Join(root, "pom.xml")); err == nil {
		// A pom outranks a package.json: a service with a web asset pipeline
		// is still built by Maven, and building only its front end would go
		// green without compiling a line of the service.
		project.Kind = KindMaven
		project.note("pom.xml found")
		if major := pomJavaVersion(string(raw)); major != "" {
			project.JavaMajor = major
			project.note("pom.xml: Java " + major)
		}
		// A parent that is not resolvable from Maven Central needs a
		// credentialed repository, which on this fork is the same upstream
		// token the private npm registry uses.
		if parent := pomParentGroup(string(raw)); parent != "" && !publicMavenGroup(parent) {
			project.PrivateRegistry = true
			project.Registry = "maven.pkg.github.com"
			project.note("pom.xml: parent " + parent + " is not a public group, so the build needs a credentialed Maven repository")
		}
	}

	// go.mod outranks both, which is the precedence this command has always
	// had: a Go repository with a JavaScript front end is a Go repository.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		project.Kind = KindGo
		project.note("go.mod found")
	}

	project.OriginOrg, project.Repo = originIdentity(root)
	return project
}

func scriptNames(scripts map[string]string) []string {
	names := make([]string, 0, len(scripts))
	for name := range scripts {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

var majorPattern = regexp.MustCompile(`(\d+)`)

// majorVersion pulls the leading major out of "20.19", "v20", ">=20 <21" or
// "^20.1.0".
func majorVersion(raw string) string {
	match := majorPattern.FindString(strings.TrimSpace(raw))
	return match
}

// authenticatedRegistryHost finds the host an .npmrc supplies a token for.
// An .npmrc that only sets a public registry needs no credential and must not
// cause a secret to be declared.
func authenticatedRegistryHost(npmrc string) string {
	for _, line := range strings.Split(npmrc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		index := strings.Index(line, ":_authToken")
		if index < 0 {
			continue
		}
		host := strings.Trim(line[:index], "/")
		if host != "" && host != "registry.npmjs.org" {
			return host
		}
	}
	return ""
}

var (
	pomJavaPattern   = regexp.MustCompile(`<(?:java\.version|maven\.compiler\.release|maven\.compiler\.source)>\s*(\d+)`)
	pomParentPattern = regexp.MustCompile(`(?s)<parent>.*?<groupId>\s*([^<\s]+)\s*</groupId>`)
)

func pomJavaVersion(pom string) string {
	if match := pomJavaPattern.FindStringSubmatch(pom); len(match) == 2 {
		return match[1]
	}
	return ""
}

func pomParentGroup(pom string) string {
	if match := pomParentPattern.FindStringSubmatch(pom); len(match) == 2 {
		return match[1]
	}
	return ""
}

// publicMavenGroup lists the parent groups that resolve from Maven Central
// without a credential. Anything else is assumed to need one, which is the
// safe direction: a declared-but-unused credential is a comment to delete,
// while a missing one is a build that fails at dependency resolution.
func publicMavenGroup(group string) bool {
	switch group {
	case "org.springframework.boot", "org.apache.maven", "org.sonatype.oss", "org.jboss", "io.quarkus":
		return true
	}
	return false
}

var originPattern = regexp.MustCompile(`url\s*=\s*\S*?[/:]([^/\s]+)/([^/\s]+?)(?:\.git)?\s*$`)

// originIdentity reads the org and repository name out of the origin remote in
// .git/config.
//
// The repository name is what a grant is keyed by, and this is where it comes
// from. The org is not: it is returned only so the generator can point out a
// disagreement with the org the server has registered, which is the one
// admission enforces. A checkout whose origin is a local path yields the
// containing directory's name here, and a secret path built from that is
// refused -- which is exactly the failure that took the org away from this
// function.
func originIdentity(root string) (org, repo string) {
	raw, err := os.ReadFile(filepath.Join(root, ".git", "config"))
	if err != nil {
		return "", ""
	}
	inOrigin := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inOrigin = trimmed == `[remote "origin"]`
			continue
		}
		if !inOrigin {
			continue
		}
		if match := originPattern.FindStringSubmatch(trimmed); len(match) == 3 {
			return match[1], match[2]
		}
	}
	return "", ""
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// --- Package manager -------------------------------------------------------

// detectPackageManager settles which tool installs dependencies.
//
// The lockfile is the evidence, not a preference. `npm ci` against a
// pnpm-lock.yaml fails for want of a package-lock.json, and against a
// yarn.lock it resolves a graph nobody has ever installed -- so a generated
// pipeline that always said `npm ci` was wrong for two of the three
// repositories it was pointed at, in one case loudly and in the other
// silently.
//
// packageManager in package.json wins when it is there, because it names an
// exact version rather than implying a range.
func detectPackageManager(root string, project *Project) {
	if _, err := os.Stat(filepath.Join(root, "pnpm-workspace.yaml")); err == nil {
		project.Workspaces = true
		project.note("pnpm-workspace.yaml: this is a workspace root")
	}

	if raw, err := os.ReadFile(filepath.Join(root, "pnpm-lock.yaml")); err == nil {
		project.Lockfile = true
		if project.PackageManager == "" {
			project.PackageManager = "pnpm"
		}
		lock := lockfileVersion(string(raw))
		if project.PackageManagerMajor == "" {
			project.PackageManagerMajor = pnpmMajorForLockfile(lock)
		}
		switch {
		case lock == "":
			project.note("pnpm-lock.yaml found, with no lockfileVersion")
		case project.PackageManagerMajor == "":
			project.note("pnpm-lock.yaml lockfileVersion " + lock + ", which no known pnpm major claims")
			project.cannot("pnpm-lock.yaml declares lockfileVersion " + lock + ", which this generator cannot map to a pnpm major. The install step runs an unpinned pnpm; pin it in package.json's packageManager field.")
		default:
			project.note("pnpm-lock.yaml lockfileVersion " + lock + ": pnpm " + project.PackageManagerMajor)
		}
		return
	}

	if _, err := os.Stat(filepath.Join(root, "yarn.lock")); err == nil {
		project.Lockfile = true
		if project.PackageManager == "" {
			project.PackageManager = "yarn"
		}
		// Berry keeps its configuration in .yarnrc.yml and takes --immutable
		// where classic takes --frozen-lockfile. Getting this wrong is an
		// unrecognized-flag error, not a wrong graph, so the check is cheap
		// and the failure it prevents is only noisy.
		if _, err := os.Stat(filepath.Join(root, ".yarnrc.yml")); err == nil {
			if project.PackageManagerMajor == "" {
				project.PackageManagerMajor = "4"
			}
			project.note("yarn.lock with .yarnrc.yml: yarn berry")
		} else {
			if project.PackageManagerMajor == "" {
				project.PackageManagerMajor = "1"
			}
			project.note("yarn.lock: yarn classic")
		}
		return
	}

	if _, err := os.Stat(filepath.Join(root, "package-lock.json")); err == nil {
		project.Lockfile = true
		if project.PackageManager == "" {
			project.PackageManager = "npm"
		}
		project.note("package-lock.json: npm")
		return
	}

	if project.Kind == KindNode && project.PackageManager == "" {
		// No lockfile at all. npm is the only tool whose install command works
		// without one, so it is the honest default -- and the note says the
		// build will resolve fresh rather than reproduce anything.
		project.PackageManager = "npm"
		project.cannot("no lockfile was found, so the install step cannot be reproducible. It runs `npm install` rather than `npm ci`; commit a lockfile.")
	}
}

// splitPackageManagerField parses "pnpm@10.4.1" or "yarn@4.1.0+sha512...".
func splitPackageManagerField(field string) (name, major string) {
	field = strings.TrimSpace(field)
	if field == "" {
		return "", ""
	}
	if plus := strings.IndexByte(field, '+'); plus >= 0 {
		field = field[:plus]
	}
	at := strings.LastIndexByte(field, '@')
	if at <= 0 {
		return "", ""
	}
	name = field[:at]
	switch name {
	case "npm", "pnpm", "yarn":
	default:
		return "", ""
	}
	return name, majorVersion(field[at+1:])
}

var lockfileVersionPattern = regexp.MustCompile(`(?m)^lockfileVersion:\s*['"]?([0-9.]+)`)

func lockfileVersion(lock string) string {
	if match := lockfileVersionPattern.FindStringSubmatch(lock); len(match) == 2 {
		return match[1]
	}
	return ""
}

// pnpmMajorForLockfile maps a pnpm lockfile format to the major that writes
// it. Format 9 is pnpm 9 and 10 both; 10 is chosen because it is the one that
// still receives releases and it reads a 9 lockfile without rewriting it.
func pnpmMajorForLockfile(version string) string {
	switch majorVersion(version) {
	case "9":
		return "10"
	case "6":
		return "8"
	case "5":
		return "7"
	default:
		return ""
	}
}
