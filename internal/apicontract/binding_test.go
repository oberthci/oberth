package apicontract_test

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"reflect"
	"testing"
)

func TestEveryDeclarationPairsExactInputAndResult(t *testing.T) {
	definitions := apicontract.Definitions(apicontract.Bindings{})
	seen := map[string]bool{}
	fields := reflect.VisibleFields(reflect.TypeFor[apicontract.Bindings]())
	for _, d := range definitions {
		if seen[d.Name] {
			t.Fatal("duplicate tool", d.Name)
		}
		seen[d.Name] = true
		var handler reflect.Type
		for _, f := range fields {
			if f.Tag.Get("tool") == d.Name {
				handler = f.Type.Field(0).Type
				break
			}
		}
		if handler == nil {
			t.Fatal("declaration has no typed handler", d.Name)
		}
		method := handler.Method(0).Type
		if d.InputType != method.In(2) || d.ResultType != method.Out(0).Out(0) {
			t.Fatal("handler and declared types differ", d.Name)
		}
		if reflect.TypeOf(d.NewInput()).Elem() != d.InputType {
			t.Fatal("decoder allocation has another input type", d.Name)
		}
	}
}
func TestTypedAdapterReturnsItsDeclaredResult(t *testing.T) {
	h := apicontract.HandlerFunc[apicontract.RunListArgs, apicontract.RunPage](func(_ context.Context, _ apicontract.Caller, input apicontract.RunListArgs) (apicontract.Call[apicontract.RunPage], error) {
		if input.Limit != 17 {
			t.Fatal("input was changed")
		}
		return func(context.Context) (apicontract.RunPage, error) {
			return apicontract.RunPage{NextCursor: "cursor"}, nil
		}, nil
	})
	d := apicontract.Binding[apicontract.RunListArgs, apicontract.RunPage]{Handler: h}.Definition(apicontract.Metadata{Name: "run_list"})
	input := d.NewInput().(*apicontract.RunListArgs)
	input.Limit = 17
	call, err := d.Prepare(t.Context(), apicontract.Caller{}, input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := call(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if reflect.TypeOf(result) != d.ResultType || result.(apicontract.RunPage).NextCursor != "cursor" {
		t.Fatal("result type or value changed", result)
	}
}

func TestDeclaredObservationTimeoutValues(t *testing.T) {
	wait := apicontract.Binding[apicontract.RunWaitArgs, apicontract.WaitResult]{}.Definition(apicontract.Metadata{Name: "run_wait"})
	if result := wait.Timeout(wait.NewInput()).(apicontract.WaitResult); !result.TimedOut || result.Run != nil {
		t.Fatal("run timeout payload differs", result)
	}
	selectorWait := apicontract.Binding[apicontract.WaitArgs, apicontract.WaitResult]{}.Definition(apicontract.Metadata{Name: "wait"})
	if result := selectorWait.Timeout(selectorWait.NewInput()).(apicontract.WaitResult); !result.TimedOut {
		t.Fatal("selector wait timeout differs", result)
	}
	promotion := apicontract.Binding[apicontract.PromoteStatusArgs, apicontract.PromotionWaitResult]{}.Definition(apicontract.Metadata{Name: "promote_status"})
	if result := promotion.Timeout(promotion.NewInput()).(apicontract.PromotionWaitResult); !result.TimedOut || result.Promotion != nil {
		t.Fatal("promotion timeout payload differs", result)
	}
	events := apicontract.Binding[apicontract.EventsWaitArgs, apicontract.EventResult]{}.Definition(apicontract.Metadata{Name: "events_wait"})
	input := events.NewInput().(*apicontract.EventsWaitArgs)
	cursor := int64(17)
	input.AfterID = &cursor
	if result := events.Timeout(input).(apicontract.EventResult); !result.TimedOut || result.NextAfterID != 17 || result.Events == nil {
		t.Fatal("event timeout lost the caller cursor", result)
	}
}
