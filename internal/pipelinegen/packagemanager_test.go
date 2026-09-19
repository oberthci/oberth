package pipelinegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkout writes a minimal repository root from name/content pairs.
func checkout(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const scriptedPackageJSON = `{"name":"x","scripts":{"test":"vitest","build":"vite build"}}`

// `npm ci` against a pnpm lockfile fails for want of a package-lock.json, and
// against a yarn.lock it resolves a graph nobody has installed. The lockfile
// is evidence, not a preference.
func TestTheLockfileDecidesThePackageManager(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		files      map[string]string
		wantTool   string
		wantMajor  string
		wantScript string
	}{
		{
			name: "pnpm 9 lockfile means pnpm 10",
			files: map[string]string{
				"package.json":   scriptedPackageJSON,
				"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
			},
			wantTool:   "pnpm",
			wantMajor:  "10",
			wantScript: "npx -y pnpm@10 install --frozen-lockfile",
		},
		{
			name: "pnpm 6 lockfile means pnpm 8",
			files: map[string]string{
				"package.json":   scriptedPackageJSON,
				"pnpm-lock.yaml": "lockfileVersion: '6.0'\n",
			},
			wantTool:   "pnpm",
			wantMajor:  "8",
			wantScript: "npx -y pnpm@8 install --frozen-lockfile",
		},
		{
			name: "yarn classic",
			files: map[string]string{
				"package.json": scriptedPackageJSON,
				"yarn.lock":    "# yarn lockfile v1\n",
			},
			wantTool:   "yarn",
			wantMajor:  "1",
			wantScript: "npx -y yarn@1 install --frozen-lockfile",
		},
		{
			name: "yarn berry takes --immutable, not --frozen-lockfile",
			files: map[string]string{
				"package.json": scriptedPackageJSON,
				"yarn.lock":    "__metadata:\n  version: 8\n",
				".yarnrc.yml":  "nodeLinker: node-modules\n",
			},
			wantTool:   "yarn",
			wantMajor:  "4",
			wantScript: "npx -y yarn@4 install --immutable",
		},
		{
			name: "npm",
			files: map[string]string{
				"package.json":      scriptedPackageJSON,
				"package-lock.json": `{"lockfileVersion":3}`,
			},
			wantTool:   "npm",
			wantScript: "npm ci --no-audit --no-fund",
		},
		{
			// The repository's own statement outranks the lockfile format,
			// because it names an exact version instead of implying a range.
			name: "packageManager field wins",
			files: map[string]string{
				"package.json":   `{"name":"x","packageManager":"pnpm@9.12.0","scripts":{"test":"vitest"}}`,
				"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
			},
			wantTool:   "pnpm",
			wantMajor:  "9",
			wantScript: "npx -y pnpm@9 install --frozen-lockfile",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project := DetectProject(checkout(t, test.files))
			if project.PackageManager != test.wantTool {
				t.Fatalf("tool = %q, want %q", project.PackageManager, test.wantTool)
			}
			if test.wantMajor != "" && project.PackageManagerMajor != test.wantMajor {
				t.Fatalf("major = %q, want %q", project.PackageManagerMajor, test.wantMajor)
			}
			yaml := Generate(project).YAML
			if !strings.Contains(yaml, test.wantScript) {
				t.Fatalf("install step is not %q:\n%s", test.wantScript, yaml)
			}
		})
	}
}

// Corepack writes its shims beside the node binary, and every pipeline
// container here runs with a read-only root filesystem. A generated pipeline
// that reached for it produced a step that failed before running anything.
func TestNoGeneratedPipelineEnablesCorepack(t *testing.T) {
	t.Parallel()
	for _, files := range []map[string]string{
		{"package.json": scriptedPackageJSON, "pnpm-lock.yaml": "lockfileVersion: '9.0'\n"},
		{"package.json": scriptedPackageJSON, "yarn.lock": "# yarn lockfile v1\n"},
		{"package.json": scriptedPackageJSON, "package-lock.json": `{"lockfileVersion":3}`},
	} {
		yaml := Generate(DetectProject(checkout(t, files))).YAML
		if strings.Contains(yaml, "corepack") {
			t.Fatalf("a generated pipeline reaches for corepack:\n%s", yaml)
		}
	}
}

// A workspace root installs every member, so the install must not be filtered
// down to one package: the repository's own scripts cannot run in that tree.
func TestAPnpmWorkspaceIsInstalledWhole(t *testing.T) {
	t.Parallel()
	project := DetectProject(checkout(t, map[string]string{
		"package.json":          scriptedPackageJSON,
		"pnpm-lock.yaml":        "lockfileVersion: '9.0'\n",
		"pnpm-workspace.yaml":   "packages:\n  - 'apps/*'\n",
		"apps/web/package.json": `{"name":"web"}`,
	}))
	if !project.Workspaces {
		t.Fatal("the workspace root was not detected")
	}
	yaml := Generate(project).YAML
	if strings.Contains(yaml, "--filter") {
		t.Fatalf("the workspace install was filtered to one package:\n%s", yaml)
	}
	if !strings.Contains(yaml, "pnpm-workspace.yaml: this is a workspace root") {
		t.Fatalf("the generated file does not say this is a workspace:\n%s", yaml)
	}
}

// A lockfile format nothing claims must not be guessed at: an unpinned pnpm is
// honest, and the header says why.
func TestAnUnknownLockfileVersionIsNotGuessedAt(t *testing.T) {
	t.Parallel()
	project := DetectProject(checkout(t, map[string]string{
		"package.json":   scriptedPackageJSON,
		"pnpm-lock.yaml": "lockfileVersion: '42'\n",
	}))
	yaml := Generate(project).YAML
	if !strings.Contains(yaml, "npx -y pnpm install --frozen-lockfile") {
		t.Fatalf("an unmappable lockfile did not fall back to an unpinned pnpm:\n%s", yaml)
	}
	if !strings.Contains(yaml, "cannot map to a pnpm major") {
		t.Fatalf("the unmapped version was not declared in the header:\n%s", yaml)
	}
}

// A Maven repository authenticates with two values, and the stored secret
// carries them as `username` and `password`. Reading only `token` failed at
// "no token was delivered" with the credential sitting in the mounted
// directory under different names.
func TestMavenSettingsTakesTheUsernameAndPasswordFields(t *testing.T) {
	t.Parallel()
	project := DetectProject(checkout(t, map[string]string{
		"pom.xml": `<project><parent><groupId>com.transferz</groupId></parent>` +
			`<properties><java.version>21</java.version></properties></project>`,
	}))
	project.Org = "transferz"
	yaml := Generate(project).YAML

	for _, want := range []string{
		"<username>${env.OBERTH_REGISTRY_USERNAME}</username>",
		"<password>${env.OBERTH_REGISTRY_PASSWORD}</password>",
		`username_file="$(oberth_secret_field username)"`,
		`password_file="$(oberth_secret_field password)"`,
	} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("generated Maven pipeline is missing %q:\n%s", want, yaml)
		}
	}
	// The credential is read from the environment, never written into the
	// build copy that later steps archive.
	if strings.Contains(yaml, "<username>x-access-token</username>") {
		t.Fatalf("the settings.xml hardcodes a username:\n%s", yaml)
	}
}
