package staticgit

import (
	"encoding/json"
	"fmt"
	"io"
)

type component struct {
	Reference  string              `json:"bom-ref"`
	Type       string              `json:"type"`
	Name       string              `json:"name"`
	Version    string              `json:"version"`
	PURL       string              `json:"purl,omitempty"`
	CPE        string              `json:"cpe,omitempty"`
	Hashes     []map[string]string `json:"hashes,omitempty"`
	Properties []map[string]string `json:"properties,omitempty"`
}

// WriteSBOM describes the three actual linked C components. Git is an upstream
// source component, never falsely labelled as an Alpine-patched Git package.
// The old-Git control uses the same CPE matcher to prove scanner coverage.
func WriteSBOM(output io.Writer, arch string, vulnerableControl bool) error {
	artifact, err := Qualified(arch)
	if err != nil {
		return err
	}
	pins := Inputs()
	version := pins["GIT_VERSION"]
	if vulnerableControl {
		version = "2.30.0"
	}
	apkArch := "x86_64"
	if arch == "arm64" {
		apkArch = "aarch64"
	}
	apk := func(name, version string) string {
		return fmt.Sprintf("pkg:apk/alpine/%s@%s?arch=%s&distro=alpine-%s", name, version, apkArch, pins["ALPINE_VERSION"])
	}
	components := []component{
		{Reference: "git", Type: "application", Name: "git", Version: version, PURL: "pkg:generic/git@" + version, CPE: "cpe:2.3:a:git-scm:git:" + version + ":*:*:*:*:*:*:*", Hashes: []map[string]string{{"alg": "SHA-256", "content": artifact.GitSHA256}}, Properties: []map[string]string{{"name": "oberth:source-sha256", "value": pins["GIT_SOURCE_SHA256"]}, {"name": "oberth:patch-sha256", "value": pins["GIT_PATCH_SHA256"]}}},
		{Reference: "musl", Type: "library", Name: "musl", Version: pins["MUSL_VERSION"], PURL: apk("musl", pins["MUSL_VERSION"]), Hashes: []map[string]string{{"alg": "SHA-256", "content": artifact.MuslArchiveSHA256}}, Properties: []map[string]string{{"name": "oberth:linked-from-apk", "value": "musl-dev"}}},
		{Reference: "zlib", Type: "library", Name: "zlib", Version: pins["ZLIB_VERSION"], PURL: apk("zlib", pins["ZLIB_VERSION"]), Hashes: []map[string]string{{"alg": "SHA-256", "content": artifact.ZlibArchiveSHA256}}, Properties: []map[string]string{{"name": "oberth:linked-from-apk", "value": "zlib-static"}}},
	}
	if vulnerableControl {
		components[0].Hashes = nil
		components[0].Properties = []map[string]string{{"name": "oberth:scanner-coverage-control", "value": "true"}}
	}
	document := map[string]any{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1, "metadata": map[string]any{"component": component{Reference: "static-git-runtime", Type: "application", Name: "oberth-static-git", Version: pins["GIT_VERSION"]}}, "components": components, "dependencies": []map[string]any{{"ref": "static-git-runtime", "dependsOn": []string{"git"}}, {"ref": "git", "dependsOn": []string{"musl", "zlib"}}}}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}
