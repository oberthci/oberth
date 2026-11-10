package api

import (
	"sync"
	"time"
)

// ToolTimeout describes a blocking tool's own server-side bound, read from
// its published input schema: the argument that carries it, in seconds, and
// the bound the server applies when that argument is omitted.
type ToolTimeout struct {
	Argument string
	Default  time.Duration
}

// toolTimeoutArguments are the spellings tools use for their own bound.
var toolTimeoutArguments = []string{"timeout", "timeout_s", "timeout_seconds"}

var toolTimeouts = sync.OnceValue(func() map[string]ToolTimeout {
	timeouts := map[string]ToolTimeout{}
	for _, definition := range toolDefinitions() {
		name, _ := definition["name"].(string)
		schema, _ := definition["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		for _, argument := range toolTimeoutArguments {
			property, ok := properties[argument].(map[string]any)
			if !ok {
				continue
			}
			bound := schemaSeconds(property["default"])
			if bound == 0 {
				bound = schemaSeconds(property["maximum"])
			}
			timeouts[name] = ToolTimeout{Argument: argument, Default: bound}
			break
		}
	}
	return timeouts
})

func schemaSeconds(value any) time.Duration {
	switch number := value.(type) {
	case int:
		return time.Duration(number) * time.Second
	case int64:
		return time.Duration(number) * time.Second
	case float64:
		return time.Duration(number * float64(time.Second))
	}
	return 0
}

// ToolTimeouts maps every tool whose schema carries its own timeout argument
// to that argument and its omitted-value bound. Clients derive their own
// deadline from it so a long poll is never cut short by a fixed default.
func ToolTimeouts() map[string]ToolTimeout {
	return toolTimeouts()
}
