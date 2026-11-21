package api

import "github.com/oberthci/oberth/internal/apicontract"

// Builtin binds the pure declaration table to its typed service handlers.
func Builtin(b apicontract.Bindings) (*Registry, error) {
	definitions := apicontract.Definitions(b)
	entries := make([]Tool, 0, len(definitions))
	for _, d := range definitions {
		entries = append(entries, bindDefinition(d))
	}
	r, err := NewRegistry(entries)
	if err != nil {
		return nil, err
	}
	r.events = b.EventsWait.Handler
	return r, nil
}
