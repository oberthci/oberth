package staticgit

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"strings"
)

// Architecture selects one qualified static runtime target.
type Architecture string

const (
	AMD64 Architecture = "amd64"
	ARM64 Architecture = "arm64"
)

func (a Architecture) Valid() bool { return a == AMD64 || a == ARM64 }

// Record binds one packaged binary and its linked archives to reviewed inputs.
// Qualified supplies the reviewed data; consumers pass records and bytes, never paths.
type Record struct {
	DistributionVersion string       `json:"distribution_version"`
	Architecture        Architecture `json:"architecture"`
	GitVersion          string       `json:"git_version"`
	GitSHA256           string       `json:"git_sha256"`
	SourceSHA256        string       `json:"source_sha256"`
	PatchSHA256         string       `json:"patch_sha256"`
	MuslVersion         string       `json:"musl_version"`
	MuslSHA256          string       `json:"musl_sha256"`
	ZlibVersion         string       `json:"zlib_version"`
	ZlibSHA256          string       `json:"zlib_sha256"`
}

// Validate refuses incomplete platform, component-version or digest records.
func (r Record) Validate() error {
	if !r.Architecture.Valid() {
		return fmt.Errorf("unsupported static binary architecture; build amd64 or arm64")
	}
	for _, version := range []string{r.DistributionVersion, r.GitVersion, r.MuslVersion, r.ZlibVersion} {
		if version == "" || len(version) > 128 || strings.ContainsAny(version, " \t\n\r") {
			return fmt.Errorf("component version is missing or invalid; supply exact build versions")
		}
	}
	for _, value := range []string{r.GitSHA256, r.SourceSHA256, r.PatchSHA256, r.MuslSHA256, r.ZlibSHA256} {
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != 32 || strings.ToLower(value) != value {
			return fmt.Errorf("component digest is missing or invalid; supply lowercase SHA-256 build digests")
		}
	}
	return nil
}

// ValidateBinary verifies the recorded bytes, target machine and absence of an
// interpreter or linked shared libraries. It reads only the supplied byte slice.
func (r Record) ValidateBinary(body []byte) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if hash(body) != r.GitSHA256 {
		return fmt.Errorf("static binary differs from its build record; rebuild and qualify the exact bytes")
	}
	file, err := elf.NewFile(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("static binary is not a readable executable; rebuild it: %w", err)
	}
	return validateExecutable(file, r.Architecture)
}

func validateExecutable(file *elf.File, architecture Architecture) error {
	machine := elf.EM_X86_64
	if architecture == ARM64 {
		machine = elf.EM_AARCH64
	}
	if file.Class != elf.ELFCLASS64 || file.Machine != machine || (file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN) {
		return fmt.Errorf("static binary target differs from its record; rebuild for the recorded architecture")
	}
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			return fmt.Errorf("static binary requires an interpreter; rebuild without dynamic runtime dependencies")
		}
	}
	libraries, err := file.ImportedLibraries()
	if err != nil || len(libraries) != 0 {
		return fmt.Errorf("static binary has unreadable or dynamic dependencies; rebuild without shared libraries")
	}
	return nil
}

// VerifyBuild checks the executable, linked archives and every reviewed builder package pin.
func (r Record) VerifyBuild(git, musl, zlib []byte, installed string) error {
	if err := r.ValidateBinary(git); err != nil {
		return err
	}
	if hash(musl) != r.MuslSHA256 || hash(zlib) != r.ZlibSHA256 {
		return fmt.Errorf("linked archive differs from the build record; rebuild with the recorded library bytes")
	}
	return verifyPackages(Inputs()["APK_PACKAGES"], installed)
}

// Labels returns a new image-label map for the recorded build inputs.
func (r Record) Labels() map[string]string {
	return map[string]string{
		"org.oberth.static-git.version":       r.GitVersion,
		"org.oberth.static-git.source-sha256": r.SourceSHA256,
		"org.oberth.static-git.musl":          r.MuslVersion,
		"org.oberth.static-git.zlib":          r.ZlibVersion,
	}
}

func hash(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
