package staticgit

import "encoding/json"

type component struct {
	Reference  string     `json:"bom-ref"`
	Type       string     `json:"type"`
	Name       string     `json:"name"`
	Version    string     `json:"version"`
	PURL       string     `json:"purl,omitempty"`
	CPE        string     `json:"cpe,omitempty"`
	Hashes     []bomHash  `json:"hashes,omitempty"`
	Properties []property `json:"properties,omitempty"`
}
type bomHash struct {
	Algorithm string `json:"alg"`
	Content   string `json:"content"`
}
type property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type dependency struct {
	Reference string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

// SBOM describes all three linked components using their actual build versions
// and hashes. The caller owns output writes and any scanner-control fixture.
func (r Record) SBOM() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	document := struct {
		Format       string       `json:"bomFormat"`
		SpecVersion  string       `json:"specVersion"`
		Version      int          `json:"version"`
		Metadata     any          `json:"metadata"`
		Components   []component  `json:"components"`
		Dependencies []dependency `json:"dependencies"`
	}{
		Format: "CycloneDX", SpecVersion: "1.6", Version: 1,
		Metadata: struct {
			Component component `json:"component"`
		}{component{Reference: "static-git-runtime", Type: "application", Name: "oberth-static-git", Version: r.GitVersion}},
		Components:   r.components(),
		Dependencies: []dependency{{"static-git-runtime", []string{"git"}}, {"git", []string{"musl", "zlib"}}},
	}
	return json.MarshalIndent(document, "", "  ")
}

func (r Record) components() []component {
	git := component{
		Reference: "git", Type: "application", Name: "git", Version: r.GitVersion,
		PURL:   "pkg:generic/git@" + r.GitVersion,
		CPE:    "cpe:2.3:a:git-scm:git:" + r.GitVersion + ":*:*:*:*:*:*:*",
		Hashes: []bomHash{{"SHA-256", r.GitSHA256}},
		Properties: []property{{"oberth:source-sha256", r.SourceSHA256},
			{"oberth:patch-sha256", r.PatchSHA256}},
	}
	return []component{git, r.library("musl", r.MuslVersion, r.MuslSHA256, "musl-dev"), r.library("zlib", r.ZlibVersion, r.ZlibSHA256, "zlib-static")}
}

func (r Record) library(name, version, digest, linkedPackage string) component {
	arch := "x86_64"
	if r.Architecture == ARM64 {
		arch = "aarch64"
	}
	return component{Reference: name, Type: "library", Name: name, Version: version,
		PURL:   "pkg:apk/alpine/" + name + "@" + version + "?arch=" + arch + "&distro=alpine-" + r.DistributionVersion,
		Hashes: []bomHash{{"SHA-256", digest}}, Properties: []property{{"oberth:linked-from-apk", linkedPackage}}}
}
