// Package staticgit binds the scratch image's C components to reviewed build inputs.
package staticgit

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

//go:embed build.env
var buildInputs string

//go:embed qualified-artifacts.json
var qualifiedArtifacts []byte

type Artifact struct {
	Architecture      string `json:"architecture"`
	GitSHA256         string `json:"git_sha256"`
	MuslArchiveSHA256 string `json:"musl_archive_sha256"`
	ZlibArchiveSHA256 string `json:"zlib_archive_sha256"`
}

func Inputs() map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(buildInputs, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			panic("invalid embedded static Git build inputs")
		}
		result[key] = strings.Trim(value, "\"")
	}
	return result
}

func Qualified(arch string) (Artifact, error) {
	var artifacts map[string]Artifact
	if err := json.Unmarshal(qualifiedArtifacts, &artifacts); err != nil {
		return Artifact{}, err
	}
	result, ok := artifacts[arch]
	if !ok || result.Architecture != arch {
		return Artifact{}, fmt.Errorf("no qualified static Git artifact for %q", arch)
	}
	return result, nil
}

func Labels() map[string]string {
	inputs := Inputs()
	return map[string]string{"org.oberth.static-git.version": inputs["GIT_VERSION"], "org.oberth.static-git.source-sha256": inputs["GIT_SOURCE_SHA256"], "org.oberth.static-git.musl": inputs["MUSL_VERSION"], "org.oberth.static-git.zlib": inputs["ZLIB_VERSION"]}
}

func ValidateBinary(arch string, body []byte) error {
	expected, err := Qualified(arch)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != expected.GitSHA256 {
		return errors.New("static Git bytes differ from the qualified component record")
	}
	file, err := elf.NewFile(bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	machine := elf.EM_X86_64
	if arch == "arm64" {
		machine = elf.EM_AARCH64
	}
	if file.Machine != machine {
		return errors.New("static Git ELF architecture mismatch")
	}
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			return errors.New("static Git contains an ELF interpreter")
		}
	}
	libraries, err := file.ImportedLibraries()
	if err != nil || len(libraries) != 0 {
		return errors.New("static Git contains dynamic dependencies")
	}
	return nil
}

// VerifyBuild refuses drift in every installed APK and both linked archives.
func VerifyBuild(arch string, git, musl, zlib []byte, installed string) error {
	if err := ValidateBinary(arch, git); err != nil {
		return err
	}
	expected, err := Qualified(arch)
	if err != nil {
		return err
	}
	for _, item := range []struct {
		body []byte
		want string
		name string
	}{{musl, expected.MuslArchiveSHA256, "musl"}, {zlib, expected.ZlibArchiveSHA256, "zlib"}} {
		digest := sha256.Sum256(item.body)
		if hex.EncodeToString(digest[:]) != item.want {
			return fmt.Errorf("linked %s archive differs from qualified build", item.name)
		}
	}
	versions := make(map[string]string)
	for _, block := range strings.Split(installed, "\n\n") {
		var name, version string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "P:") {
				name = line[2:]
			}
			if strings.HasPrefix(line, "V:") {
				version = line[2:]
			}
		}
		if name != "" {
			versions[name] = version
		}
	}
	for _, pin := range strings.Fields(Inputs()["APK_PACKAGES"]) {
		name, want, ok := strings.Cut(pin, "=")
		if !ok || versions[name] != want {
			return fmt.Errorf("build package %s does not match its exact pin", name)
		}
	}
	return nil
}
