// A native static fixture for host-driver and QMP lifecycle integration tests.
package main

import (
	"encoding/json"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	args := os.Args[1:]
	if args[0] == "--case" {
		id := args[1]
		if id == "timeout" {
			time.Sleep(time.Minute)
		}
		if id == "fail" {
			os.Stdout.WriteString(`{"all":"passed"}`)
			os.Exit(1)
		}
		socket := args[5]
		deadline := time.Now().Add(2 * time.Second)
		for {
			conn, e := net.Dial("unix", socket)
			if e == nil {
				var result map[string]any
				_ = json.NewDecoder(conn).Decode(&result)
				conn.Close()
				break
			}
			if time.Now().After(deadline) {
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
