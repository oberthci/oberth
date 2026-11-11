package api

import (
	"sync"
	"time"
)

// ToolTimeout describes a blocking tool's own server-side bound, read from
// its published input schema: the argument that carries it, in seconds, and
// the bound the server applies when that argument is omitted.
type ToolTimeout struct {
	// Argument is the spelling every server release accepts for this tool,
	// the one a client uses when it fills the bound in itself.
	Argument string
	// Aliases are the other spellings the schema accepts for the same bound.
	Aliases []string
	Default time.Duration
}

// toolTimeoutWireSpelling names, for tools that published another spelling
// before timeout_s, the spelling older servers accept.
var toolTimeoutWireSpelling = map[string]string{"wait": "timeout", "promote_status": "timeout", "run_hold_exec": "timeout_seconds"}

// ToolTimeoutArguments are the spellings tools use for their own bound,
// the documented one (timeout_s) first.
var ToolTimeoutArguments = []string{"timeout_s", "timeout", "timeout_seconds"}

var toolTimeouts = sync.OnceValue(func() map[string]ToolTimeout {
	timeouts := map[string]ToolTimeout{}
	for _, definition := range toolDefinitions() {
		name, _ := definition["name"].(string)
		schema, _ := definition["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		var timeout ToolTimeout
		for _, argument := range ToolTimeoutArguments {
			property, ok := properties[argument].(map[string]any)
			if !ok {
				continue
			}
			if timeout.Argument != "" {
				timeout.Aliases = append(timeout.Aliases, argument)
				continue
			}
			bound := schemaSeconds(property["default"])
			if bound == 0 {
				bound = schemaSeconds(property["maximum"])
			}
			timeout.Argument, timeout.Default = argument, bound
		}
		if timeout.Argument == "" {
			continue
		}
		if wire := toolTimeoutWireSpelling[name]; wire != "" && wire != timeout.Argument {
			for index, alias := range timeout.Aliases {
				if alias == wire {
					timeout.Aliases[index], timeout.Argument = timeout.Argument, wire
				}
			}
		}
		timeouts[name] = timeout
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
