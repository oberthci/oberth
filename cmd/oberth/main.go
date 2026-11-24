// Command oberth exposes the installation's tool table to people and scripts.
//
// Spec: docs/plans/c11-cli-and-server-composition.md#commands-and-output
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/oberthci/oberth/internal/client"
)

type application struct {
	in              io.Reader
	out, diagnostic io.Writer
	base            string
	env             func(string) string
	config          client.Config
	server          *serverDependencies
	directory       string
	git             func(context.Context, []string) (string, error)
	install         installationDispatch
	secretExec      installationDispatch
	executable      func() (string, error)
}

func main() {
	_ = logging.NewSlogLoggerCustom(logging.Error, logging.JSON, io.Discard)
	ctx, cancel := interruptContext(context.Background())
	defer cancel()
	app := application{in: os.Stdin, out: os.Stdout, diagnostic: os.Stderr, env: os.Getenv}
	if code, handled := app.helperEntry(ctx, os.Args); handled {
		os.Exit(code)
	}
	if len(os.Args) > 2 && os.Args[1] == "secretstore" && os.Args[2] == "exec" {
		os.Exit(app.run(ctx, os.Args[1:]))
	}
	base, err := profileBase(os.Getenv, os.UserHomeDir)
	if err != nil {
		m := messages()["config"]
		_, _ = fmt.Fprintln(os.Stderr, m[0]+"\n"+m[1])
		os.Exit(1)
	}
	app.base = base
	os.Exit(app.runBinary(ctx, os.Args))
}

func profileBase(env func(string) string, home func() (string, error)) (string, error) {
	if base := env("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "oberth"), nil
	}
	base, err := home()
	return filepath.Join(base, ".config", "oberth"), err
}

func (a application) run(ctx context.Context, args []string) (code int) {
	if code, ok := a.entry(ctx, args); ok {
		return code
	}
	a.out = terminalOutput(a.out)
	if writer, ok := a.out.(*terminalWriter); ok {
		defer func() {
			if writer.Flush() != nil {
				code = 1
			}
		}()
	}
	return a.runCommand(ctx, args)
}

func (a application) runCommand(ctx context.Context, args []string) int {
	if code, ok := a.flow(ctx, args); ok {
		return code
	}
	options, rest, problems := parseCommon(args)
	rows := commands()
	if catalogueRequested(options, rest, problems) {
		return a.catalogue()
	}
	if helpRequested(options, rest) {
		return a.help(rows, rest, problems)
	}
	if len(rest) == 2 && rest[0] == "tool" && rest[1] == "list" {
		return a.toolList(rows, problems)
	}
	row, tail, ok := findCommand(rows, rest)
	if !ok {
		problems = append(problems, unknownCommand(rest))
	}
	var input []byte
	if ok {
		input, problems = toolArgs(row, tail, a.in, problems)
	}
	if len(problems) > 0 {
		if ok {
			problems = append(problems, "Run oberth help "+row.path+" for this command.")
		}
		return a.usage(problems)
	}
	return a.remote(ctx, row, input, options)
}

func (a application) remote(ctx context.Context, row command, input []byte, options commonOptions) int {
	resolved, err := a.resolveRemote(row, input, options.transport)
	if err != nil {
		return a.failedDetail(err, options.detail)
	}
	cfg := a.config
	cfg.Options = resolved
	cfg.Diagnostic = a.diagnostic
	remote, err := client.New(cfg)
	if err != nil {
		return a.failedDetail(err, options.detail)
	}
	defer remote.Close()
	paths, err := client.ReserveCredentials(a.base, row.definition, resolved)
	if err != nil {
		return a.failedDetail(err, options.detail)
	}
	defer client.CleanupCredentials(paths)
	if row.definition.LongPoll {
		if err = writeWrapped(a.diagnostic, waitingFor(row.definition, input)); err != nil {
			return a.failedDetail(err, options.detail)
		}
	}
	data, refused, err := callRemote(ctx, remote, remoteCall{row.definition, input, options.bearerFile})
	if err != nil {
		var problems usageProblems
		if errors.As(err, &problems) {
			return a.usage(problems)
		}
		return a.failedDetail(err, options.detail)
	}
	if !refused {
		data, err = client.SaveCredentials(data, paths)
		if err != nil {
			return a.failedDetail(err, options.detail)
		}
	}
	if err = a.render(data, refused, row.resultOptions(options, refused)); err != nil {
		return a.failedDetail(err, options.detail)
	}
	if refused {
		return 1
	}
	return 0
}

func (a application) toolList(rows []command, problems []string) int {
	if len(problems) > 0 {
		return a.usage(problems)
	}
	for _, row := range rows {
		if _, err := fmt.Fprintln(a.out, row.definition.Name); err != nil {
			return 1
		}
	}
	return 0
}

// The first signal starts graceful shutdown; the next forces exit even when
// the launching process gave this process an ignored interrupt disposition.
func interruptContext(parent context.Context) (context.Context, context.CancelFunc) {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(parent)
	stopped := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { signal.Stop(signals); close(stopped); cancel() }) }
	go func() {
		defer signal.Stop(signals)
		select {
		case <-signals:
			cancel()
		case <-parent.Done():
			cancel()
			return
		case <-stopped:
			return
		}
		select {
		case received := <-signals:
			code := 130
			if received == syscall.SIGTERM {
				code = 143
			}
			os.Exit(code)
		case <-stopped:
		}
	}()
	return ctx, stop
}
