package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/oberthci/oberth/internal/staticgit"
)

type match struct {
	Artifact      struct{ Name, Version string } `json:"artifact"`
	Vulnerability struct{ ID, Severity string }  `json:"vulnerability"`
}
type report struct {
	Matches    []match `json:"matches"`
	Ignored    []match `json:"ignoredMatches"`
	Descriptor struct {
		Name, Version string
		DB            struct{ Status struct{ Valid bool } }
	} `json:"descriptor"`
	Source struct{ Type, Target string } `json:"source"`
}

func severe(match match) bool {
	return strings.EqualFold(match.Vulnerability.Severity, "high") || strings.EqualFold(match.Vulnerability.Severity, "critical")
}
func read(path string, value any) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, (32<<20)+1))
	if err != nil {
		return "", err
	}
	if len(body) > 32<<20 {
		return "", errors.New("static component metadata exceeds bound")
	}
	if err := json.Unmarshal(body, value); err != nil {
		return "", err
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:]), nil
}
func validateReport(value report, target string) error {
	if value.Matches == nil || value.Descriptor.Name != "grype" || value.Descriptor.Version != staticgit.Inputs()["GRYPE_VERSION"] || !value.Descriptor.DB.Status.Valid || value.Source.Type != "sbom-file" || filepath.Clean(value.Source.Target) != filepath.Clean(target) {
		return errors.New("static scan omitted a valid pinned scanner, database, results or SBOM source")
	}
	return nil
}
func check(arch, sbomPath, scanPath, controlPath string, output io.Writer) error {
	artifact, err := staticgit.Qualified(arch)
	if err != nil {
		return err
	}
	var expected bytes.Buffer
	if err := staticgit.WriteSBOM(&expected, arch, false); err != nil {
		return err
	}
	var actualSBOM, expectedSBOM any
	sbomHash, err := read(sbomPath, &actualSBOM)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(expected.Bytes(), &expectedSBOM); err != nil {
		return err
	}
	if !reflect.DeepEqual(actualSBOM, expectedSBOM) {
		return errors.New("static SBOM differs from qualified C component identities and hashes")
	}
	pins := staticgit.Inputs()
	var actual, control report
	scanHash, err := read(scanPath, &actual)
	if err != nil {
		return err
	}
	controlHash, err := read(controlPath, &control)
	if err != nil {
		return err
	}
	if err := validateReport(actual, sbomPath); err != nil {
		return err
	}
	if err := validateReport(control, filepath.Join(filepath.Dir(sbomPath), "static-git-control.cdx.json")); err != nil {
		return err
	}
	for _, match := range actual.Matches {
		if severe(match) {
			return errors.New("static C scan contains HIGH/CRITICAL vulnerabilities")
		}
	}
	if len(actual.Ignored) != 0 {
		return errors.New("static C scan suppressed vulnerability matches")
	}
	coverage := 0
	for _, match := range control.Matches {
		if match.Artifact.Name == "git" && match.Artifact.Version == "2.30.0" && severe(match) {
			coverage++
		}
	}
	if coverage == 0 {
		return errors.New("scanner failed the known-vulnerable source Git coverage control")
	}
	return json.NewEncoder(output).Encode(map[string]any{"status": "PASS", "architecture": arch, "scanner": "Grype " + pins["GRYPE_VERSION"], "components": []string{"git", "musl", "zlib"}, "git_binary_sha256": artifact.GitSHA256, "sbom_sha256": sbomHash, "scan_sha256": scanHash, "control_sha256": controlHash, "control_git_high_critical_matches": coverage, "scanner_descriptor": actual.Descriptor})
}
func run() error {
	arch := flag.String("arch", "", "qualified architecture")
	sbomPath := flag.String("sbom", "", "actual SBOM")
	scanPath := flag.String("scan", "", "actual scan")
	controlPath := flag.String("control", "", "known-vulnerable Git scan")
	flag.Parse()
	return check(*arch, *sbomPath, *scanPath, *controlPath, os.Stdout)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
