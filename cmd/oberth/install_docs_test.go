package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func installDocSection(t *testing.T, heading string) string {
	t.Helper()
	data, err := os.ReadFile("../../docs/install.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(data), heading+"\n")
	if !ok {
		t.Fatal("missing section", heading)
	}
	section, _, _ = strings.Cut(section, "\n## ")
	return section
}

func docShell(t *testing.T, section string) string {
	t.Helper()
	blocks := strings.Split(section, "```sh\n")[1:]
	if len(blocks) == 0 {
		t.Fatal("missing shell command")
	}
	var scripts []string
	for _, block := range blocks {
		script, _, ok := strings.Cut(block, "\n```")
		if !ok {
			t.Fatal("unclosed shell command")
		}
		scripts = append(scripts, script)
	}
	return strings.Join(scripts, "\n")
}

func TestStoreOperatorGuidesMatchRequiredInputs(t *testing.T) {
	for _, path := range []string{"../../docs/install.md", "../../docs/operations.md"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(data)), " ")
		for _, required := range []string{"Install configures the bundled store", "operator Pod", "OpenBao", "administrator profile", "Kubernetes login token"} {
			if !strings.Contains(text, required) {
				t.Fatal(path, "missing", required)
			}
		}
		for _, command := range []string{"secretstore check", "secretstore setup"} {
			flags := installationFlags(command)
			for _, name := range []string{"config", "ca-file", "token-file"} {
				if flags.Lookup(name) == nil || !strings.Contains(text, "`--"+name+"`") {
					t.Fatal(path, command, name)
				}
			}
		}
		if strings.Contains(text, "Never print the recovery file. Check configuration with") {
			t.Fatal("profile-based check instruction survived")
		}
	}
}

func TestDocumentedRemovalTargetsOnlySelectedInstallation(t *testing.T) {
	section := installDocSection(t, "## Remove an installation")
	for _, required := range []string{"no uninstall command", "Helm directly", "Shared workflow definitions stay", "local profile", "pipeline namespace", "Pre-existing namespaces stay", "Keep a pre-existing shared namespace", "After preserving its recovery material", "private signing key", "creation mark", "pre-existing pipeline namespace keeps step ServiceAccounts and run objects", "never by name patterns", "Administrator public keys", "Helm never owned the five Secrets"} {
		if !strings.Contains(strings.Join(strings.Fields(section), " "), required) {
			t.Fatal("missing removal consequence", required)
		}
	}
	for _, cleanup := range []string{"Installation-created namespace", "Retained namespace"} {
		t.Run(cleanup, func(t *testing.T) { checkDocumentedRemoval(t, section, cleanup) })
	}
}

func checkDocumentedRemoval(t *testing.T, section, cleanup string) {
	t.Helper()
	dir := removalCommandFixture(t)
	profile := filepath.Join(dir, "selected")
	survivor := filepath.Join(dir, "survivor")
	for _, path := range []string{profile, survivor} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	common, _, _ := strings.Cut(section, "\n### ")
	script := docShell(t, common) + "\n" + removalCleanup(t, section, cleanup) + "\n" + removalCleanup(t, section, "Retained pipeline namespace") + "\n" + removalCleanup(t, section, "Local profile")
	script = strings.ReplaceAll(script, "/absolute/path/to/installation-profile", "'"+strings.ReplaceAll(profile, "'", "'\"'\"'")+"'")
	calls := filepath.Join(dir, "calls")
	command := exec.Command("sh", "-ec", script)
	command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "context=chosen", "namespace=second", "pipeline_namespace=second-runs", "owner=oberth/second", "releases=", "release_lookup_exit=0", "calls="+calls)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	want := "helm\nmark\nreleases\nnamespace\nschedules\nruns\naccounts\n"
	if cleanup == "Retained namespace" {
		want = "helm\nclaims\nsecrets\nschedules\nruns\naccounts\n"
	}
	data, err := os.ReadFile(calls)
	if err != nil || string(data) != want {
		t.Fatal("wrong removal scope or order", string(data), err)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatal("profile not removed", err)
	}
	if _, err := os.Stat(survivor); err != nil {
		t.Fatal("other profile removed", err)
	}
}

func removalCleanup(t *testing.T, section, heading string) string {
	t.Helper()
	_, part, ok := strings.Cut(section, "### "+heading+"\n")
	if !ok {
		t.Fatal("missing cleanup case", heading)
	}
	part, _, _ = strings.Cut(part, "\n### ")
	return docShell(t, part)
}

func removalCommandFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	names := renderedRemovalNames(t)
	for name, body := range map[string]string{
		"helm": `#!/bin/sh
[ "$*" = '--kube-context chosen uninstall oberth -n second --wait' ] || exit 1
printf 'helm\n' >> "$calls"
`,
		"kubectl": fmt.Sprintf(`#!/bin/sh
claims='%s'
secrets='%s'
case "$*" in
  '--context chosen get namespace second -o jsonpath={.metadata.annotations.meta\.helm\.sh/release-name}{"/"}{.metadata.annotations.meta\.helm\.sh/release-namespace}')
    printf '%%s' "$owner"
    action=mark ;;
  '--context chosen -n second-runs delete serviceaccounts -l app.kubernetes.io/managed-by=oberth') action=accounts ;;
  '--context chosen -n second-runs delete cronworkflows.argoproj.io -l app.kubernetes.io/managed-by=oberth,workflows.argoproj.io/controller-instanceid=second') action=schedules ;;
  '--context chosen -n second-runs delete workflows.argoproj.io -l oberth.ci/engine=argo,workflows.argoproj.io/controller-instanceid=second') action=runs ;;
  '--context chosen -n second get secrets -l owner=helm -o name')
    printf 'releases\n' >> "$calls"
    printf '%%s' "$releases"
    exit "${release_lookup_exit:-0}" ;;
  '--context chosen delete namespace second') action=namespace ;;
  "--context chosen -n second delete pvc $claims") action=claims ;;
  "--context chosen -n second delete secret $secrets") action=secrets ;;
  *) exit 1 ;;
esac
printf '%%s\n' "$action" >> "$calls"
`, strings.Join(names["PVC"], " "), strings.Join(names["Secret"], " ")),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDocumentedArgoDefinitionOwnerLifecycle(t *testing.T) {
	section := installDocSection(t, "## A second installation in the same cluster")
	_, policy, ok := strings.Cut(section, "Removing the Argo release")
	if !ok {
		t.Fatal("missing shared-definition lifecycle policy")
	}
	policy = strings.Join(strings.Fields("Removing the Argo release"+policy), " ")
	for _, required := range []string{
		"keeps them by resource policy", "every run object depends on those definitions",
		"chart defaults replaces the definitions' contents", "release notes name as compatible",
		"Install and upgrade refuse workflow definitions that would drop fields this version submits",
		"after their owner upgrades them, run `oberth upgrade --dry-run`",
	} {
		if !strings.Contains(policy, required) {
			t.Fatal("missing definition-owner obligation", required)
		}
	}
	if strings.Contains(policy, "refuses to start runs") {
		t.Fatal("policy promises a runtime compatibility check", policy)
	}
}

func TestDocumentedRemovalKeepsUnmarkedAndForeignNamespaces(t *testing.T) {
	section := installDocSection(t, "## Remove an installation")
	for _, owner := range []string{"", "/", "other/second", "oberth/first"} {
		dir := removalCommandFixture(t)
		calls := filepath.Join(dir, "calls")
		command := exec.Command("sh", "-ec", removalCleanup(t, section, "Installation-created namespace"))
		command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "context=chosen", "namespace=second", "owner="+owner, "calls="+calls)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal(err, string(output))
		}
		data, err := os.ReadFile(calls)
		if err != nil || string(data) != "mark\n" {
			t.Fatal("unowned namespace deleted", owner, string(data), err)
		}
	}
}

func TestDocumentedRemovalKeepsNamespaceWithHelmReleasesOrFailedLookup(t *testing.T) {
	section := installDocSection(t, "## Remove an installation")
	for _, test := range []struct{ name, releases, exit string }{
		{"foreign release", "secret/sh.helm.release.v1.other.v1", "0"},
		{"failed lookup", "", "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := removalCommandFixture(t)
			calls := filepath.Join(dir, "calls")
			command := exec.Command("sh", "-ec", removalCleanup(t, section, "Installation-created namespace"))
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "context=chosen", "namespace=second", "owner=oberth/second", "releases="+test.releases, "release_lookup_exit="+test.exit, "calls="+calls)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatal(err, string(output))
			}
			data, err := os.ReadFile(calls)
			if err != nil || string(data) != "mark\nreleases\n" {
				t.Fatal("namespace deleted without an empty successful Helm release lookup", string(data), err)
			}
		})
	}
}
