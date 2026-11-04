package service

import (
	"errors"
	"fmt"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// SizeWeights is the operator's admission weight per size (#1021). XL = 0
// means "the whole budget": an XL run is admitted only once nothing else
// holds weight and then runs alone. A positive XL is an ordinary weight, so an
// XL run shares the host with whatever fits beside it.
type SizeWeights struct {
	S, M, L, XL int
}

// DefaultSizeWeights is the behaviour before weights were configurable.
var DefaultSizeWeights = SizeWeights{S: 1, M: 2, L: 3, XL: 0}

// Validate requires every weight to be at least 1, except XL, which may be 0
// (the whole budget), and S <= M <= L <= XL.
func (weights SizeWeights) Validate() error {
	var problems []error
	for _, size := range []struct {
		name   string
		weight int
	}{{"S", weights.S}, {"M", weights.M}, {"L", weights.L}} {
		if size.weight < 1 {
			problems = append(problems, fmt.Errorf("size weight %s=%d must be at least 1", size.name, size.weight))
		}
	}
	if weights.XL < 0 {
		problems = append(problems, fmt.Errorf("size weight XL=%d must be 0 (the whole budget) or at least 1", weights.XL))
	}
	if weights.S > weights.M || weights.M > weights.L {
		problems = append(problems, fmt.Errorf("size weights must satisfy S <= M <= L <= XL, got S=%d M=%d L=%d", weights.S, weights.M, weights.L))
	}
	if weights.XL > 0 && weights.XL < weights.L {
		problems = append(problems, fmt.Errorf("size weights must satisfy L <= XL, got L=%d XL=%d (XL=0 means the whole budget)", weights.L, weights.XL))
	}
	return errors.Join(problems...)
}

// String renders the weights in the flag's own syntax.
func (weights SizeWeights) String() string {
	return fmt.Sprintf("S=%d,M=%d,L=%d,XL=%d", weights.S, weights.M, weights.L, weights.XL)
}

// wholeBudget reports whether XL takes the whole budget and runs alone.
func (weights SizeWeights) wholeBudget(size pipeline.Size) bool {
	return size.Effective() == pipeline.XL && weights.XL == 0
}

// of returns a size's weight against a budget, clamped to it; XL = 0 is the
// whole budget.
func (weights SizeWeights) of(size pipeline.Size, capacity int) int {
	weight := 0
	switch size.Effective() {
	case pipeline.S:
		weight = weights.S
	case pipeline.M:
		weight = weights.M
	case pipeline.L:
		weight = weights.L
	case pipeline.XL:
		weight = weights.XL
		if weight == 0 {
			weight = capacity
		}
	}
	return min(weight, capacity)
}

// sizeRank orders sizes for comparisons that are about the declaration, not
// the configured weight (size grants, preflight sizing).
func sizeRank(size pipeline.Size) int {
	switch size.Effective() {
	case pipeline.S:
		return 1
	case pipeline.M:
		return 2
	case pipeline.L:
		return 3
	case pipeline.XL:
		return 4
	default:
		return 0
	}
}
