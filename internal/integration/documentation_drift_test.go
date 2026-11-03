package integration_test

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestDocumentationLocalLinksExist(t *testing.T) {
	root := filepath.Join("..", "..")
	paths := []string{"README.md", "BACKLOG.md"}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".md") {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			paths = append(paths, relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	link := regexp.MustCompile(`\]\(([^\s)]+)\)`)
	for _, path := range paths {
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range link.FindAllSubmatch(body, -1) {
			target, err := url.Parse(string(match[1]))
			if err != nil {
				t.Errorf("%s: %v", path, err)
				continue
			}
			if target.IsAbs() || target.Host != "" || target.Path == "" {
				continue
			}
			resolved := filepath.Join(root, filepath.Dir(path), target.Path)
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s: broken local link %s", path, match[1])
			}
		}
	}
}

func TestArchitectureMapPathsExist(t *testing.T) {
	root := filepath.Join("..", "..")
	body, err := os.ReadFile(filepath.Join(root, ".aimap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Subsystems map[string]struct {
			Paths []string `yaml:"paths"`
		} `yaml:"subsystems"`
		Symptoms map[string][]string `yaml:"symptom_index"`
	}
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	for key, subsystem := range document.Subsystems {
		document.Symptoms[key] = subsystem.Paths
	}
	for key, paths := range document.Symptoms {
		for _, path := range paths {
			if _, err := os.Stat(filepath.Join(root, path)); err != nil {
				t.Errorf("%s: nonexistent map path %s", key, path)
			}
		}
	}
}
