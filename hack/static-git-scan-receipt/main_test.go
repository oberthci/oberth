package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/staticgit"
)

func TestReceiptRequiresCompleteQualifiedScan(t *testing.T) {
	for _, fault := range []string{"valid", "empty-report", "wrong-source", "wrong-scanner", "invalid-db", "missing-component", "wrong-library-hash", "high", "ignored-high", "missing-control"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			sbom := filepath.Join(dir, "static-git-amd64.cdx.json")
			file, err := os.Create(sbom)
			if err != nil {
				t.Fatal(err)
			}
			if err := staticgit.WriteSBOM(file, "amd64", false); err != nil {
				t.Fatal(err)
			}
			file.Close()
			var actual, control report
			actual.Matches = []match{}
			actual.Source.Type, actual.Source.Target = "sbom-file", sbom
			actual.Descriptor.Name, actual.Descriptor.Version = "grype", staticgit.Inputs()["GRYPE_VERSION"]
			actual.Descriptor.DB.Status.Valid = true
			control = actual
			control.Source.Target = filepath.Join(dir, "static-git-control.cdx.json")
			var vulnerable match
			vulnerable.Artifact.Name, vulnerable.Artifact.Version, vulnerable.Vulnerability.Severity = "git", "2.30.0", "High"
			control.Matches = []match{vulnerable}
			switch fault {
			case "empty-report":
				actual = report{}
			case "wrong-source":
				actual.Source.Target = "different.cdx.json"
			case "wrong-scanner":
				actual.Descriptor.Version = "0.0.0"
			case "invalid-db":
				actual.Descriptor.DB.Status.Valid = false
			case "high":
				actual.Matches = []match{vulnerable}
			case "ignored-high":
				actual.Ignored = []match{vulnerable}
			case "missing-control":
				control.Matches = []match{}
			case "missing-component", "wrong-library-hash":
				body, _ := os.ReadFile(sbom)
				var value map[string]any
				if err := json.Unmarshal(body, &value); err != nil {
					t.Fatal(err)
				}
				components := value["components"].([]any)
				if fault == "missing-component" {
					value["components"] = components[:2]
				} else {
					components[1].(map[string]any)["hashes"] = []any{}
				}
				body, _ = json.Marshal(value)
				if err := os.WriteFile(sbom, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for name, value := range map[string]report{"scan.json": actual, "control.json": control} {
				body, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = check("amd64", sbom, filepath.Join(dir, "scan.json"), filepath.Join(dir, "control.json"), io.Discard)
			if (err == nil) != (fault == "valid") {
				t.Fatalf("fault=%s err=%v", fault, err)
			}
		})
	}
}
