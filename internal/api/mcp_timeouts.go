package api

import (
	"sync"
	"time"
)

// ToolTimeoutArgument is the one argument every blocking tool takes for its
// own server-side bound, in seconds.
const ToolTimeoutArgument = "timeout_s"

// ToolTimeout describes a blocking tool's own server-side bound, read from
// its published input schema: the bound the server applies when
// timeout_s is omitted.
type ToolTimeout struct {
	Default time.Duration
}

var toolTimeouts = sync.OnceValue(func() map[string]ToolTimeout {
	timeouts := map[string]ToolTimeout{}
	for _, definition := range toolDefinitions() {
		name, _ := definition["name"].(string)
		schema, _ := definition["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		property, ok := properties[ToolTimeoutArgument].(map[string]any)
		if !ok {
			continue
		}
		bound := schemaSeconds(property["default"])
		if bound == 0 {
			bound = schemaSeconds(property["maximum"])
		}
		timeouts[name] = ToolTimeout{Default: bound}
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

// ToolTimeouts maps every tool whose schema carries timeout_s to its
// omitted-value bound. Clients derive their own deadline from it so a long
// poll is never cut short by a fixed default.
func ToolTimeouts() map[string]ToolTimeout {
	return toolTimeouts()
}

// FixedToolBounds are the server-side bounds of tools that take no timeout
// argument. run_hold_patch reads and then writes the held step's tree through
// the server's helper, each bounded like run_hold_exec by assist.execTimeout.
func FixedToolBounds() map[string]time.Duration {
	return map[string]time.Duration{"run_hold_patch": 2 * toolTimeouts()["run_hold_exec"].Default}
}

// toolWriteBound is how long a non-long-poll tool may take before its reply
// is written; zero keeps the server's WriteTimeout.
func toolWriteBound(name string) time.Duration {
	if bound, ok := FixedToolBounds()[name]; ok {
		return bound
	}
	return toolTimeouts()[name].Default
}
