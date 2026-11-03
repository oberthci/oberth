package model

const MaxPromotionSources = 32

// PromotionSource retains request order and the immutable resolution of a train
// member. A skipped source remains in the record; it is never silently omitted.
type PromotionSource struct {
	SHA           string   `json:"sha"`
	Ref           string   `json:"ref,omitempty"`
	GrantID       string   `json:"grant_id,omitempty"`
	Skipped       bool     `json:"skipped,omitempty"`
	ConflictPaths []string `json:"conflict_paths,omitempty"`
}

type PromotionTrain struct {
	Sources         []PromotionSource `json:"sources"`
	SkipConflicting bool              `json:"skip_conflicting,omitempty"`
	TreeSHA         string            `json:"tree_sha,omitempty"`
}

func (p Promotion) SourceCount() int {
	if p.Train != nil {
		return len(p.Train.Sources)
	}
	return 1
}
