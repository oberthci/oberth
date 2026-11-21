package staticgit

import (
	"encoding/json"
	"fmt"
	"strings"
)

type artifact struct {
	Architecture Architecture `json:"architecture"`
	GitSHA256    string       `json:"git_sha256"`
	MuslSHA256   string       `json:"musl_archive_sha256"`
	ZlibSHA256   string       `json:"zlib_archive_sha256"`
}

// Inputs returns an independent copy of the reviewed build and scanner pins.
func Inputs() map[string]string {
	input := make(map[string]string)
	for _, line := range strings.Split(buildInputs, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			panic("reviewed static build inputs are invalid")
		}
		input[key] = strings.Trim(value, "\"")
	}
	return input
}

// Qualified returns the one reviewed packaging record for a supported target.
func Qualified(arch Architecture) (Record, error) {
	var artifacts map[Architecture]artifact
	if err := json.Unmarshal([]byte(qualifiedArtifacts), &artifacts); err != nil {
		panic("reviewed static artifacts are invalid")
	}
	artifact, ok := artifacts[arch]
	if !ok || artifact.Architecture != arch {
		return Record{}, fmt.Errorf("unsupported static binary platform; choose amd64 or arm64")
	}
	input := Inputs()
	record := Record{Architecture: arch, DistributionVersion: input["ALPINE_VERSION"],
		GitVersion: input["GIT_VERSION"], GitSHA256: artifact.GitSHA256, SourceSHA256: input["GIT_SOURCE_SHA256"], PatchSHA256: input["GIT_PATCH_SHA256"],
		MuslVersion: input["MUSL_VERSION"], MuslSHA256: artifact.MuslSHA256, ZlibVersion: input["ZLIB_VERSION"], ZlibSHA256: artifact.ZlibSHA256}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	return record, nil
}
