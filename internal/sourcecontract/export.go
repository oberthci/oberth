package sourcecontract

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/gitoid"
)

const (
	MaxFileBytes = 1 << 20
	MaxRefs      = 4096
)

// Ref retains a public ref's raw object and optional peeled commit.
// Tags can name trees/blobs; branch heads always identify a commit.
type Ref struct {
	Name      string `json:"ref"`
	ObjectSHA string `json:"object_sha"`
	CommitSHA string `json:"commit_sha"`
}

func (r Ref) Validate() error {
	if err := ValidatePublicRef(r.Name); err != nil {
		return err
	}
	if !gitoid.Valid(r.ObjectSHA) {
		return invalid("ref.object_sha", "exact object required")
	}
	if r.CommitSHA != "" {
		if !gitoid.Valid(r.CommitSHA) {
			return invalid("ref.commit_sha", "exact commit required when present")
		}
	} else if strings.HasPrefix(r.Name, "refs/heads/") {
		return invalid("ref.commit_sha", "branch head must identify its commit")
	}
	return nil
}

// Export freezes the main checkout's public ref view at admission. Zero Ref or
// DefaultBranch means absent. No other branch heads are delivered; raw tag
// objects, including tags on trees/blobs, remain part of this snapshot and are
// encoded in ascending name order. Tags retain their own objects; Keep pins only
// the tested commit, own ref and default head, and a load fails closed with a
// clear error if a recorded tag is gone.
type Export struct {
	Ref           Ref   `json:"ref"`
	DefaultBranch Ref   `json:"default_branch"`
	Tags          []Ref `json:"tags"`
}

func (e Export) Clone() Export { e.Tags = slices.Clone(e.Tags); return e }

func (e Export) MarshalJSON() ([]byte, error) {
	type wire Export
	e = e.Clone()
	slices.SortFunc(e.Tags, func(a, b Ref) int { return strings.Compare(a.Name, b.Name) })
	return json.Marshal(wire(e))
}

func (e Export) Validate() error {
	if e.Ref != (Ref{}) {
		if err := e.Ref.Validate(); err != nil {
			return err
		}
	}
	if e.DefaultBranch != (Ref{}) {
		if err := e.DefaultBranch.Validate(); err != nil {
			return err
		}
		if !strings.HasPrefix(e.DefaultBranch.Name, "refs/heads/") {
			return invalid("export.default_branch", "a branch head is required")
		}
	}
	if len(e.Tags) > MaxRefs {
		return invalid("export.tags", "at most 4096 tags are allowed")
	}
	seen := make(map[string]bool)
	for _, tag := range e.Tags {
		if err := tag.Validate(); err != nil {
			return err
		}
		if !strings.HasPrefix(tag.Name, "refs/tags/") {
			return invalid("export.tags", "only tags are included")
		}
		if seen[tag.Name] {
			return invalid("export.tags", "duplicate tag name")
		}
		seen[tag.Name] = true
	}
	return nil
}

// ValidatePublicRef accepts exact public heads/tags and refuses revision syntax.
func ValidatePublicRef(value string) error {
	prefix := "refs/heads/"
	if strings.HasPrefix(value, "refs/tags/") {
		prefix = "refs/tags/"
	}
	if !strings.HasPrefix(value, prefix) || len(strings.TrimPrefix(value, prefix)) == 0 || len(strings.TrimPrefix(value, prefix)) > 255 {
		return invalid("ref", "public head or tag with a 1..255 byte suffix required")
	}
	if err := validateRefSyntax(value); err != nil {
		return err
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") || strings.HasSuffix(segment, ".") {
			return invalid("ref", "ambiguous ref component")
		}
	}
	return nil
}

func validateRefSyntax(value string) error {
	if strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.ContainsAny(value, " ~^:?*[\\") {
		return invalid("ref", "revision syntax refused")
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return invalid("ref", "control characters refused")
		}
	}
	return nil
}
