package sourcecontract

import (
	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/reponame"
)

type Revision struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
}

func (r Revision) Validate() error {
	if _, err := reponame.ParseRepository(r.Repository); err != nil {
		return invalid("revision.repository", "canonical repository required")
	}
	if !gitoid.Valid(r.Commit) {
		return invalid("revision.commit", "full lowercase object identifier required")
	}
	return nil
}

// Kind selects the main admitted source or one pinned producer revision.
type Kind string

const (
	Main   Kind = "main"
	Pinned Kind = "pin"
)

func (k Kind) Valid() bool { return k == Main || k == Pinned }

type Descriptor struct {
	Version    int    `json:"version"`
	Kind       Kind   `json:"kind"`
	Repository string `json:"repository,omitempty"`
	Commit     string `json:"commit,omitempty"`
}

func (d Descriptor) Validate() error {
	if d.Version != 1 {
		return invalid("descriptor.version", "must equal 1")
	}
	switch d.Kind {
	case Main:
		if d.Repository != "" || d.Commit != "" {
			return invalid("descriptor", "main has no repository or commit")
		}
		return nil
	case Pinned:
		return (Revision{Repository: d.Repository, Commit: d.Commit}).Validate()
	default:
		return invalid("descriptor.kind", "must be main or pin")
	}
}
