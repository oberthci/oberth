package argoworkflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"io"
	"path"
	"reflect"
	"regexp"
	"strings"
)

const SourceArtifactKey = "{{workflow.parameters.oberth-source}}"

// SourceInput is an authored declaration, with no source-download authority.
type SourceInput struct {
	Template   string                    `json:"template"`
	Artifact   string                    `json:"artifact"`
	Path       string                    `json:"path"`
	Descriptor sourcecontract.Descriptor `json:"descriptor"`
	Shared     bool                      `json:"shared"`
}

// SourceInputInventory reports effective source declarations, including shared snapshots.
type SourceInputInventory struct {
	Inputs     []SourceInput `json:"inputs"`
	UsesSource bool          `json:"uses_source"`
}

func artifactArgument(a wfv1.Artifact) bool {
	return reflect.DeepEqual(a, wfv1.Artifact{Name: a.Name, From: a.From})
}
func descriptor(a wfv1.Artifact) (sourcecontract.Descriptor, bool) {
	if a.Plugin == nil {
		return sourcecontract.Descriptor{}, false
	}
	if a.Plugin.Name != "oberth" || a.Plugin.Key != SourceArtifactKey {
		return sourcecontract.Descriptor{}, false
	}
	decoder := json.NewDecoder(strings.NewReader(a.Plugin.Configuration))
	decoder.DisallowUnknownFields()
	var d sourcecontract.Descriptor
	if decoder.Decode(&d) != nil || d.Validate() != nil {
		return sourcecontract.Descriptor{}, false
	}
	var trailing any
	if !errors.Is(decoder.Decode(&trailing), io.EOF) {
		return sourcecontract.Descriptor{}, false
	}
	// Native descriptor JSON is strict about duplicates as well as unknown fields.
	raw, err := oneDocument([]byte(a.Plugin.Configuration))
	var fields map[string]any
	if err != nil || !bytes.HasPrefix(raw, []byte("{")) || json.Unmarshal(raw, &fields) != nil || !exactKeys(fields, []string{"version", "kind", "repository", "commit"}) {
		return sourcecontract.Descriptor{}, false
	}
	return d, true
}
func (g *graph) artifacts(n *node) {
	t := n.template
	seen := map[string]bool{}
	for _, a := range t.Inputs.Artifacts {
		at := n.path + ".inputs.artifacts[" + a.Name + "]"
		if !literal(a.Name) || seen[a.Name] {
			g.refuse(at, "artifact")
		}
		seen[a.Name] = true
		if a.Path != "" && (parentPath(a.Path) || reservedPath(a.Path)) {
			g.refuse(at+".path", "path")
		}
		if artifactOverrides(a) {
			g.refuse(at, "artifact")
		}
		g.clusterReads(a.ArtifactLocation, n, at)
		g.sourceArtifact(a, at)
	}
	// Clean output paths are tidiness, not the token boundary: symlinks remain.
	for _, a := range t.Outputs.Artifacts {
		if parentPath(a.Path) {
			g.refuse(n.path+".outputs.artifacts["+a.Name+"].path", "path")
		}
		g.clusterReads(a.ArtifactLocation, n, n.path+".outputs.artifacts["+a.Name+"]")
	}
}
func sourceDestination(a wfv1.Artifact, d sourcecontract.Descriptor, leaf bool) bool {
	if !leaf && a.Path == "" {
		return true
	}
	if a.Mode == nil || *a.Mode != 0755 || a.SubPath != "" || a.RecurseMode {
		return false
	}
	if d.Kind == sourcecontract.Main {
		return a.Path == "/work/src"
	}
	name := strings.TrimPrefix(a.Path, "/work/sources/")
	if name == a.Path || len(name) == 0 || len(name) > 128 || path.Clean(a.Path) != a.Path {
		return false
	}
	return regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`).MatchString(name)
}
func (g *graph) resultReferences(inv *sourceInventory) {
	for _, n := range g.nodes {
		raw, err := json.Marshal(n.template)
		if err != nil {
			continue
		}
		text := string(raw)
		for _, c := range n.calls {
			if c.target == nil || !inv.takesSource(c.target) {
				continue
			}
			if resultReference(text, c.name) {
				g.refuse(c.path+".outputs.result", "source_result")
			}
		}
	}
}
func resultReference(text, name string) bool {
	quoted := regexp.QuoteMeta(name)
	task := `(?:tasks|steps)\s*(?:\.\s*` + quoted + `|\[\s*['"]` + quoted + `['"]\s*\])`
	outputs := `\s*(?:\.\s*outputs|\[\s*['"]outputs['"]\s*\])`
	result := `\s*(?:\.\s*result|\[\s*['"]result['"]\s*\])(?:[^a-zA-Z0-9_]|$)`
	pattern := regexp.MustCompile(task + outputs + result)
	decoder := json.NewDecoder(strings.NewReader(text))
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		if value, ok := token.(string); ok && pattern.MatchString(value) {
			return true
		}
	}
}
func sourcePath(n *node, a wfv1.Artifact) string {
	return fmt.Sprintf("%s.inputs.artifacts[%q]", n.path, a.Name)
}
func leaf(t *wfv1.Template) bool {
	return t.Container != nil || t.Script != nil || t.ContainerSet != nil || t.Resource != nil || t.Suspend != nil
}
func (g *graph) sourceInputs() SourceInputInventory {
	result := SourceInputInventory{Inputs: []SourceInput{}}
	for _, n := range g.nodes {
		for _, a := range n.template.Inputs.Artifacts {
			if d, ok := descriptor(a); ok {
				result.Inputs = append(result.Inputs, SourceInput{Template: n.key, Artifact: a.Name, Path: a.Path, Descriptor: d, Shared: n.scope != ""})
				result.UsesSource = true
			}
		}
	}
	return result
}
func artifactOverrides(a wfv1.Artifact) bool {
	return a.From != "" || a.FromExpression != "" || a.ArtifactGC != nil || a.GlobalName != "" || a.SubPath != "" || a.RecurseMode || a.Deleted
}

func sourceRootDestination(value string) bool {
	value = path.Clean(value)
	if value == "/work/src" {
		return true
	}
	name := strings.TrimPrefix(value, "/work/sources/")
	return name != value && name != "" && !strings.Contains(name, "/")
}

func (g *graph) sourceArtifact(a wfv1.Artifact, at string) {
	if a.Plugin == nil {
		origin := a.ArtifactLocation
		origin.ArchiveLogs = nil
		if !reflect.DeepEqual(origin, wfv1.ArtifactLocation{}) && sourceRootDestination(a.Path) {
			g.refuse(at, "source")
		}
		return
	}
	remaining := a.ArtifactLocation
	remaining.Plugin = nil
	remaining.ArchiveLogs = nil
	if !reflect.DeepEqual(remaining, wfv1.ArtifactLocation{}) {
		g.refuse(at, "artifact")
	}
	if _, ok := descriptor(a); !ok {
		g.refuse(at, "source")
	}
}
