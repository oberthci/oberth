package nativebinding

import (
	"time"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

// Preparation supplies admitted repository facts that TransformInput does not carry.
type Preparation struct {
	Repository, File string
	RepositoryID     int64
	Ref              sourcecontract.Ref
	Trigger          string
	Release          bool
	Assist           bool
	HoldTTL          time.Duration
	HelperImage      string
}
