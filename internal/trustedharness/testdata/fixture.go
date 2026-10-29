// A native static fixture for host-driver and QMP lifecycle integration tests.
package main

import (
	"encoding/json"
	"net"
	"os"
	"strings"
	"time"
)

// Overridden by the integration test's build to match its timeout scale.
// The host-driver environment is intentionally scrubbed by the harness.
var readinessTimeout = "30s"

func main() {
	args := os.Args[1:]
	if args[0] == "--case" {
		id := args[1]
		if id == "timeout" {
			// Only caller cancellation may finish this case; a longer test
			// watchdog must not turn the synthetic stall into a success.
			for {
				time.Sleep(time.Minute)
			}
		}
		if id == "fail" {
			os.Stdout.WriteString(`{"all":"passed"}`)
			os.Exit(1)
		}
		socket := args[5]
		// Both readiness phases share this test-only scheduling budget, well
		// inside the integration test's scaled parent watchdog.
		budget, err := time.ParseDuration(readinessTimeout)
		if err != nil || budget <= 0 {
			os.Stderr.WriteString("invalid fixture readiness budget\n")
			os.Exit(2)
		}
		deadline := time.Now().Add(budget)
		for {
			conn, e := net.Dial("unix", socket)
			if e == nil {
				var result map[string]any
				_ = json.NewDecoder(conn).Decode(&result)
				conn.Close()
				break
			}
			if time.Now().After(deadline) {
				os.Stderr.WriteString("fixture guest-control readiness deadline exceeded\n")
				os.Exit(2)
			}
			time.Sleep(time.Millisecond)
		}
		// A guest response can arrive before the fixture has bound QMP. Do not
		// finish the case until shutdown can reach the monitor socket.
		for {
			info, e := os.Stat(args[3])
			if e == nil && info.Mode()&os.ModeSocket != 0 {
				break
			}
			if time.Now().After(deadline) {
				os.Stderr.WriteString("fixture QMP readiness deadline exceeded\n")
				os.Exit(2)
			}
			time.Sleep(time.Millisecond)
		}
		// Guest-controlled result text is deliberately not interpreted as inventory.
		return
	}
	var qmp, guest string
	for n := 1; n < len(args); n++ {
		if args[n] == "-qmp" {
			qmp = strings.Split(strings.TrimPrefix(args[n+1], "unix:"), ",")[0]
		}
		if args[n] == "-chardev" {
			for _, v := range strings.Split(args[n+1], ",") {
				if strings.HasPrefix(v, "path=") {
					guest = strings.TrimPrefix(v, "path=")
				}
			}
		}
	}
	gl, e := net.Listen("unix", guest)
	if e != nil {
		os.Exit(3)
	}
	defer gl.Close()
	go func() {
		for {
			c, e := gl.Accept()
			if e != nil {
				return
			}
			_ = json.NewEncoder(c).Encode(map[string]string{"cases": "invented guest PASS"})
			c.Close()
		}
	}()
	ql, e := net.Listen("unix", qmp)
	if e != nil {
		os.Exit(4)
	}
	defer ql.Close()
	c, e := ql.Accept()
	if e != nil {
		os.Exit(5)
	}
	defer c.Close()
	enc := json.NewEncoder(c)
	_ = enc.Encode(map[string]any{"QMP": map[string]any{}})
	dec := json.NewDecoder(c)
	for {
		var req map[string]string
		if dec.Decode(&req) != nil {
			os.Exit(6)
		}
		_ = enc.Encode(map[string]any{"return": map[string]any{}})
		if req["execute"] == "quit" {
			return
		}
	}
}
