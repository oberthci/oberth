// Command oberth-store-backup exports and verifies encrypted offline database archives.
// Spec: docs/plans/c7-durable-state.md#baseline-and-backup
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

type config struct {
	mode, database, output, recipient, input, identity string
	options                                            stateport.Options
	progress                                           func()
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(runCommand(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func runCommand(ctx context.Context, args []string, out, diagnostic io.Writer) int {
	if err := run(ctx, args, out); err != nil {
		_, _ = fmt.Fprintln(diagnostic, recoveryDiagnostic(err))
		return 1
	}
	return 0
}

func parse(args []string) (config, error) {
	c := config{mode: "backup"}
	if len(args) > 0 && (args[0] == "verify" || args[0] == "restore") {
		c.mode = args[0]
		args = args[1:]
	}
	f := flag.NewFlagSet("oberth-store-backup", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&c.database, "database", "", "existing database")
	f.StringVar(&c.output, "output", "", "absent output path")
	f.StringVar(&c.recipient, "recipient", "", "age X25519 public recipient")
	f.StringVar(&c.input, "input", "", "encrypted input archive")
	f.StringVar(&c.identity, "identity", "", "private age X25519 identity file")
	f.StringVar(&c.options.Protocol.SchemaIdentity, "schema-identity", "", "exact schema identity")
	f.StringVar(&c.options.Protocol.AuditDomain, "audit-domain", "", "exact audit domain")
	f.IntVar(&c.options.Schema, "expect-schema", 0, "exact supported schema")
	f.Int64Var(&c.options.MaxBytes, "max-database-bytes", 0, "logical database limit")
	if err := f.Parse(args); err != nil {
		return c, recoveryError(argumentsInvalid, err)
	}
	if f.NArg() != 0 || c.options.Schema != store.Schema {
		return c, refused()
	}
	return c, c.options.Validate()
}

func run(ctx context.Context, args []string, out io.Writer) error {
	c, err := parse(args)
	if err != nil {
		return err
	}
	ctx, touch, stop := idleContext(ctx, idleLimit, startIdleTimer)
	defer stop()
	c.progress = touch
	if c.mode == "backup" {
		err = encrypt(ctx, c, out)
	} else {
		err = decrypt(ctx, c, out)
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

type receipt struct {
	Format string `json:"format"`
	stateport.BackupReport
	CipherSHA   string `json:"ciphertext_sha256"`
	CipherBytes int64  `json:"ciphertext_bytes"`
	Recipient   string `json:"recipient,omitempty"`
	Encryption  string `json:"encryption"`
}

func emitReceipt(ctx context.Context, out io.Writer, r receipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(r); err != nil {
		return err
	}
	return ctx.Err()
}
