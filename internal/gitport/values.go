package gitport

import "slices"

// PromotionRequest binds one exact tested source and an exact target base.
// An empty target object means the target must still be absent.
type PromotionRequest struct {
	Repository    string   `json:"repository"`
	Source        Revision `json:"source"`
	Target        Ref      `json:"target"`
	ActionReceipt string   `json:"action_receipt"`
}

// Promotion retains exact tested source, base, result and conflict paths.
// An empty Result with nonempty Conflicts is evidence, not a publishable result.
type Promotion struct {
	Request   PromotionRequest `json:"request"`
	Result    Ref              `json:"result"`
	Tree      string           `json:"tree"`
	Conflicts []string         `json:"conflicts"`
}

func (p Promotion) Clone() Promotion {
	p.Conflicts = slices.Clone(p.Conflicts)
	return p
}

// PublicationReceipt records exact changed or already-delivered refs, not authority.
type PublicationReceipt struct {
	Repository    string `json:"repository"`
	ActionReceipt string `json:"action_receipt"`
	Refs          []Ref  `json:"refs"`
}

func (r PublicationReceipt) Clone() PublicationReceipt { r.Refs = slices.Clone(r.Refs); return r }

func (f File) Clone() File { f.Bytes = slices.Clone(f.Bytes); return f }
