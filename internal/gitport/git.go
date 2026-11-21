package gitport

import (
	"context"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

type Git interface {
	// ReadFile returns detached exact regular-file bytes, at most sourcecontract.MaxFileBytes.
	// Missing file/ref/commit is AbsentError; cancellation and I/O are UnavailableError.
	ReadFile(context.Context, Revision, string) (File, error)
	// ResolveRef reads one exact public local ref, preserving object and peeled commit identity.
	ResolveRef(context.Context, string, string) (Ref, error)
	// ListRefs returns at most sourcecontract.MaxRefs exact public refs in ascending name order.
	ListRefs(context.Context, string) ([]Ref, error)
	// RemoteRef observes an exact upstream ref; unavailable never means absent.
	RemoteRef(context.Context, string, string) (Ref, error)
	// PreparePromotion retains the exact source and target base without publishing refs.
	PreparePromotion(context.Context, PromotionRequest) (Promotion, error)
}

// Revision shares the frozen source revision representation.
type Revision = sourcecontract.Revision

type File struct {
	Path  string `json:"path"`
	Blob  string `json:"blob"`
	Mode  uint32 `json:"mode"`
	Bytes []byte `json:"bytes"`
}

type Ref = sourcecontract.Ref
