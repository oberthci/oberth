// oberth-kvm-harness runs exclusively in the administrator-pinned trusted leaf.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/oberthci/oberth/internal/trustedharness"
	"github.com/oberthci/oberth/internal/vmrunner"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	if len(args) != 2 || args[0] != "--intent" || len(args[1]) > 32768 {
		return fmt.Errorf("expected bounded --intent")
	}
	b, e := base64.RawURLEncoding.DecodeString(args[1])
	if e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var intent vmrunner.KVMIntent
	if e = d.Decode(&intent); e != nil {
		return e
	}
	var extra any
	if e = d.Decode(&extra); !errors.Is(e, io.EOF) {
		return fmt.Errorf("unexpected intent data")
	}
	report, e := trustedharness.Run(ctx, intent, "/work/artifacts", "/trusted")
	if e != nil {
		return e
	}
	b, e = json.Marshal(report)
	if e != nil {
		return e
	}
	if len(b) > 4096 {
		return fmt.Errorf("termination report exceeds kubelet limit")
	}
	return os.WriteFile("/dev/termination-log", b, 0600)
}
