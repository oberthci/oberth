package service

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/pkg/periapsis"
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

// ParseSizeWeights reads "S=1,M=2,L=3,XL=0". Sizes it does not name keep
// their default; an unknown size, a duplicate or a non-integer is refused.
func ParseSizeWeights(value string) (SizeWeights, error) {
	weights := DefaultSizeWeights
	if strings.TrimSpace(value) == "" {
		return weights, nil
	}
	seen := map[string]bool{}
	for _, entry := range strings.Split(value, ",") {
		key, raw, found := strings.Cut(entry, "=")
		key, raw = strings.TrimSpace(key), strings.TrimSpace(raw)
		if !found || key == "" || raw == "" {
			return SizeWeights{}, fmt.Errorf("size weight %q is not SIZE=WEIGHT", strings.TrimSpace(entry))
		}
		if seen[key] {
			return SizeWeights{}, fmt.Errorf("size weight %s is given more than once", key)
		}
		seen[key] = true
		weight, err := strconv.Atoi(raw)
		if err != nil {
			return SizeWeights{}, fmt.Errorf("size weight %s=%q is not an integer", key, raw)
		}
		switch periapsis.Size(key) {
		case periapsis.S:
			weights.S = weight
		case periapsis.M:
			weights.M = weight
		case periapsis.L:
			weights.L = weight
		case periapsis.XL:
			weights.XL = weight
		default:
			return SizeWeights{}, fmt.Errorf("size weight names unknown size %q; sizes are S, M, L and XL", key)
		}
	}
	return weights, weights.Validate()
}

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
func (weights SizeWeights) wholeBudget(size periapsis.Size) bool {
	return size.Effective() == periapsis.XL && weights.XL == 0
}

// of returns a size's weight against a budget, clamped to it; XL = 0 is the
// whole budget.
func (weights SizeWeights) of(size periapsis.Size, capacity int) int {
	weight := 0
	switch size.Effective() {
	case periapsis.S:
		weight = weights.S
	case periapsis.M:
		weight = weights.M
	case periapsis.L:
		weight = weights.L
	case periapsis.XL:
		weight = weights.XL
		if weight == 0 {
			weight = capacity
		}
	}
	return min(weight, capacity)
}

// sizeRank orders sizes for comparisons that are about the declaration, not
// the configured weight (size grants, preflight sizing).
func sizeRank(size periapsis.Size) int {
	switch size.Effective() {
	case periapsis.S:
		return 1
	case periapsis.M:
		return 2
	case periapsis.L:
		return 3
	case periapsis.XL:
		return 4
	default:
		return 0
	}
}
