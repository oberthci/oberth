package publicerr

import (
	"errors"
	"reflect"
	"slices"
	"strings"
)

// Classify follows the package's boundary rule, returning nil only for nil input.
// Same-code reason/remedy pairs are deduplicated and listed in sorted order.
// The returned error retains the original cause tree without rendering it.
func Classify(err error) *Error {
	if err == nil {
		return nil
	}
	var tree classification
	tree.visit(err, 0, false)
	selected := tree.refusals[0]
	for _, candidate := range tree.refusals[1:] {
		if candidate.err.Code() == Invariant {
			selected = candidate
			break
		}
		if selected.err.Code() != Invariant && candidate.depth < selected.depth {
			selected = candidate
		}
	}
	id := selected.err.ID
	var messages []message
	var details, contexts []string
	for _, entry := range tree.refusals {
		if entry.err.Code() == selected.err.Code() {
			if entry.err.ID != id {
				id = ""
			}
			messages = append(messages, entry.err.messages()...)
			contexts = append(contexts, entry.err.Context...)
			if entry.err != nil && entry.err.Detail != "" {
				details = append(details, entry.err.Detail)
			}
		}
	}
	slices.SortFunc(messages, func(a, b message) int {
		if order := strings.Compare(a.reason, b.reason); order != 0 {
			return order
		}
		return strings.Compare(a.remedy, b.remedy)
	})
	slices.Sort(details)
	slices.Sort(contexts)
	return &Error{ID: id, Context: slices.Compact(contexts), Detail: strings.Join(slices.Compact(details), "\n"), code: selected.err.Code(), details: slices.Compact(messages), cause: err}
}

// Permanent reports whether every refusal in err's tree is the caller's to fix.
func Permanent(err error) bool {
	if err == nil || nilError(err) {
		return false
	}
	var tree classification
	tree.visit(err, 0, false)
	for _, refusal := range tree.refusals {
		switch refusal.err.Code() {
		case InvalidInput, NotFound, Forbidden:
		default:
			return false
		}
	}
	return true
}

type refusal struct {
	err   *Error
	depth int
}
type classification struct{ refusals []refusal }

func (c *classification) visit(err error, depth int, labeled bool) {
	if nilError(err) {
		c.refusals = append(c.refusals, refusal{unknown(err), depth})
		return
	}
	var public *Error
	// A descendant is classified at its own depth, never as this caller's boundary.
	if reflect.TypeOf(err) == reflect.TypeFor[*Error]() && errors.As(err, &public) {
		c.refusals = append(c.refusals, refusal{public, depth})
		labeled = true
	}
	if children, ok := err.(interface{ Unwrap() []error }); ok {
		found := false
		for _, child := range children.Unwrap() {
			if child != nil {
				c.visit(child, depth+1, labeled)
				found = true
			}
		}
		if found {
			return
		}
	}
	if wrapper, ok := err.(interface{ Unwrap() error }); ok {
		if child := wrapper.Unwrap(); child != nil {
			c.visit(child, depth+1, labeled)
			return
		}
	}
	if !labeled {
		c.refusals = append(c.refusals, refusal{unknown(err), depth})
	}
}
func nilError(err error) bool {
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	}
	return false
}
