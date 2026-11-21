package main

import (
	"bufio"
	"context"
	"io"
	"slices"
	"sync"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/setuptui"
	"k8s.io/client-go/tools/clientcmd"
)

func (s *installationSession) prompt(ctx context.Context, in io.Reader, out io.Writer) error {
	s.input = bufio.NewScanner(in)
	s.interactivePrompt = installationTerminal(in)
	if s.request.name != "install" || s.choices.Context != "" || s.request.enabled("dry-run") || s.request.enabled("yes") {
		return nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = s.app.env("KUBECONFIG")
	config, err := rules.Load()
	if err != nil {
		return installer.NewProblem("cluster", err, "Kubeconfig: "+rules.ExplicitPath)
	}
	contexts := make([]string, 0, len(config.Contexts))
	for name := range config.Contexts {
		contexts = append(contexts, name)
	}
	slices.Sort(contexts)
	choices, err := setuptui.Ask(ctx, s.input, out, contexts)
	if err != nil {
		return err
	}
	s.choices.Context = choices.Context
	if s.choices.URL == "" {
		s.choices.URL = choices.URL
	}
	if s.name == "" {
		s.name = defaultInstallationProfile(choices.Context, s.choices.Namespace)
	}
	return nil
}

func watchInstallationInput(ctx context.Context, in io.Reader) func() {
	closer, ok := in.(io.Closer)
	if !ok {
		return func() {}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	closeInput := sync.OnceFunc(func() { _ = closer.Close() })
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			closeInput()
		case <-stop:
		}
	}()
	return func() {
		if ctx.Err() != nil {
			closeInput()
		}
		close(stop)
		<-done
	}
}
