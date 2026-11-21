package apicontract

import (
	"context"
	"reflect"
)

// Binding is a statically checked service implementation for one declaration.
type Binding[I, O any] struct{ Handler Handler[I, O] }

// Definition carries the declared I/O types and their typed service adapter.
// NewInput supplies the only pointer accepted by Prepare and Timeout.
type Definition struct {
	Metadata
	InputType, ResultType reflect.Type
	Bound                 bool
	NewInput              func() any
	Prepare               func(context.Context, Caller, any) (Call[any], error)
	Timeout               func(any) any
}

func (b Binding[I, O]) Definition(m Metadata) Definition {
	return Definition{Metadata: m, InputType: reflect.TypeFor[I](), ResultType: reflect.TypeFor[O](), Bound: bound(b.Handler), NewInput: func() any { return new(I) }, Prepare: func(ctx context.Context, c Caller, input any) (Call[any], error) {
		call, err := b.Handler.Prepare(ctx, c, *input.(*I))
		if err != nil {
			return nil, err
		}
		if call == nil {
			return nil, nil
		}
		return func(ctx context.Context) (any, error) { return call(ctx) }, nil
	}, Timeout: func(input any) any { return timeoutValue[I, O](*input.(*I)) }}
}
func bound(value any) bool {
	if value == nil {
		return false
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Interface:
		return !v.IsNil()
	}
	return true
}
func timeoutValue[I, O any](input I) O {
	if v, ok := any(input).(interface{ timeoutResult() O }); ok {
		return v.timeoutResult()
	}
	var output O
	if v, ok := any(&output).(interface{ waitTimedOut() }); ok {
		v.waitTimedOut()
	}
	return output
}
