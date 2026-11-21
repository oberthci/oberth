package sourcecontract

import (
	"path"
	"strings"

	"github.com/oberthci/oberth/internal/gitoid"
)

// Receipt records the selected document at an exact revision. DocumentDigest
// binds authored bytes; the prepared admission owns the native spec digest.
type Receipt struct {
	Version        int      `json:"version"`
	Revision       Revision `json:"revision"`
	ObjectSHA      string   `json:"object_sha"`
	File           string   `json:"file"`
	Blob           string   `json:"blob"`
	DocumentDigest string   `json:"document_digest"`
}

func (r Receipt) Clone() Receipt { return r }

// ScheduledLocator is the source locator every child of a schedule carries.
// One sealed parent serves many runs, so it cannot name one; the source
// endpoint selects the run named by the caller's own Workflow instead.
const ScheduledLocator = "scheduled"

// Binding is an exact source declaration, immutable once sealed in prepared
// admission. Revision and Export describe the frozen checkout, not authority
// to load it. Clone detaches the tag snapshot.
//
// Spec: docs/plans/c6-source-delivery.md#owned-data-and-public-contract
type Binding struct {
	Template            string     `json:"template"`
	Artifact            string     `json:"artifact"`
	Path                string     `json:"path"`
	Descriptor          Descriptor `json:"descriptor"`
	RepositoryID        int64      `json:"repository_id"`
	Revision            Revision   `json:"revision"`
	Export              Export     `json:"export"`
	AuthorizationDigest string     `json:"authorization_digest"`
}

func (b Binding) Clone() Binding { b.Export = b.Export.Clone(); return b }

func (r Receipt) Validate(document []byte) error {
	if r.Version != 1 {
		return invalid("receipt.version", "must equal 1")
	}
	if err := r.Revision.Validate(); err != nil {
		return err
	}
	if err := ValidateFilePath(r.File); err != nil {
		return err
	}
	for _, value := range []string{r.ObjectSHA, r.Blob} {
		if !gitoid.Valid(value) {
			return invalid("receipt", "exact object and file blob required")
		}
	}
	if len(document) == 0 || len(document) > MaxFileBytes || r.DocumentDigest != Digest(document) {
		return invalid("receipt.document_digest", "exact nonempty document of at most 1 MiB required")
	}
	return nil
}

func (b Binding) Validate() error {
	if b.Template == "" || b.Artifact == "" {
		return invalid("binding", "template and artifact identity required")
	}
	if b.RepositoryID < 1 {
		return invalid("binding.repository_id", "durable repository identity required")
	}
	if err := b.Revision.Validate(); err != nil {
		return err
	}
	if err := b.Descriptor.Validate(); err != nil {
		return err
	}
	if b.AuthorizationDigest != "" && !validDigest(b.AuthorizationDigest) {
		return invalid("binding.authorization_digest", "lowercase SHA-256 required")
	}
	if b.Descriptor.Kind == Main {
		return b.validateMain()
	}
	return b.validatePin()
}

func (b Binding) validateMain() error {
	if b.Path != "/work/src" {
		return invalid("binding.path", "main destination must be /work/src")
	}
	if err := b.Export.Validate(); err != nil {
		return err
	}
	if b.Export.Ref != (Ref{}) && b.Export.Ref.CommitSHA != b.Revision.Commit {
		return invalid("binding.export.ref", "must identify the tested commit")
	}
	return nil
}

func (b Binding) validatePin() error {
	if b.Revision.Repository != b.Descriptor.Repository || b.Revision.Commit != b.Descriptor.Commit {
		return invalid("binding.revision", "must equal the admitted pin descriptor")
	}
	if !validDigest(b.AuthorizationDigest) {
		return invalid("binding.authorization_digest", "pin authorization required")
	}
	if b.Export.Ref != (Ref{}) || b.Export.DefaultBranch != (Ref{}) || len(b.Export.Tags) != 0 {
		return invalid("binding.export", "pins contain no Git metadata")
	}
	return validatePinPath(b.Path)
}

func validatePinPath(value string) error {
	const prefix = "/work/sources/"
	name := strings.TrimPrefix(value, prefix)
	if name == value || name == "" || strings.Contains(name, "/") {
		return invalid("pin.path", "one named destination under /work/sources required")
	}
	if path.Clean(value) != value || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "\x00\r\n\\") {
		return invalid("pin.path", "a clean destination name without dot prefixes or control characters required")
	}
	return nil
}

// ValidateFilePath refuses traversal, metadata paths and ambiguous spellings.
func ValidateFilePath(value string) error {
	if value == "" || len(value) > 4096 || path.Clean(value) != value || strings.HasPrefix(value, "/") || value == "." {
		return invalid("file", "clean relative path of at most 4096 bytes required")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." || strings.EqualFold(segment, ".git") || strings.ContainsAny(segment, "\x00\r\n\\") {
			return invalid("file", "metadata, traversal and control characters refused")
		}
	}
	return nil
}
