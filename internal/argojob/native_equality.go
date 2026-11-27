package argojob

import (
	"reflect"
	"slices"

	"github.com/oberthci/oberth/internal/stateport"
)

// Raw prose is independent evidence. Churn in it cannot rewrite public steps,
// allocate another evidence generation or emit a progress event.
func sameNodeFacts(a, b []stateport.Node) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	for i := range a {
		a[i].Message = ""
	}
	for i := range b {
		b[i].Message = ""
	}
	return reflect.DeepEqual(a, b)
}
