package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/secretstore"
)

func openCommandStore(r installationRequest) (*secretstore.Client, secretstore.Config, error) {
	if r.text("config") == "" {
		return nil, secretstore.Config{}, installationProblem("Secret-store configuration is missing.", "Pass --config with the installation configuration and its one store audience.")
	}
	c, err := readConfig(r.text("config"))
	if err != nil {
		return nil, secretstore.Config{}, err
	}
	cfg := storeConfiguration(c)
	if err = cfg.Validate(); err != nil {
		return nil, cfg, err
	}
	ca, err := client.ReadPublic(r.text("ca-file"))
	if err != nil {
		return nil, cfg, err
	}
	address, err := url.Parse(c.SecretStore.Address)
	if err != nil {
		return nil, cfg, err
	}
	store, err := secretstore.New(secretstore.Connection{URL: c.SecretStore.Address, ServerName: address.Hostname(), CA: ca})
	return store, cfg, err
}

func runStoreOperator(ctx context.Context, r installationRequest, out io.Writer) error {
	store, cfg, err := openCommandStore(r)
	if err != nil {
		return err
	}
	defer store.Close()
	if r.enabled("backup-only") && r.text("unseal-file") != "" {
		if err = setupBackupWithUnseal(ctx, r, store, cfg); err != nil {
			return err
		}
		_, err = io.WriteString(out, "Secret-store backup access configured.\n")
		return err
	}

	token, err := client.ReadPrivate(r.text("token-file"))
	if err != nil {
		return err
	}
	defer clear(token)
	if r.name == "secretstore setup" && r.enabled("backup-only") {
		err = store.SetupBackup(ctx, token, cfg)
	} else if r.name == "secretstore setup" {
		err = store.Setup(ctx, token, cfg)
	} else {
		err = store.Check(ctx, token, cfg)
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, "Secret-store roles and audience verified.\n")
	return err
}

func setupBackupWithUnseal(ctx context.Context, r installationRequest, store *secretstore.Client, cfg secretstore.Config) error {
	if r.text("token-file") != "" {
		return installationProblem("Choose one backup setup authority.", "Use --unseal-file for the bundled store, or --token-file for a delegated store administrator.")
	}
	share, err := client.ReadPrivate(r.text("unseal-file"))
	if err != nil {
		return err
	}
	defer clear(share)
	return store.SetupBackupWithUnseal(ctx, cfg, share)
}

func (a application) runSecretStep(ctx context.Context, r installationRequest, in io.Reader, out io.Writer) error {
	inputs, err := a.secretStepEnvironment()
	if err != nil {
		return err
	}
	store, err := secretstore.New(inputs.connection)
	if err != nil {
		return err
	}
	defer store.Close()
	token, err := projectedStoreToken(inputs.token)
	if err != nil {
		return err
	}
	defer clear(token)
	delivery := secretstore.Delivery{Parent: inputs.scratch, Token: token, Paths: inputs.paths, Run: func(ctx context.Context, dir string) error {
		command := exec.CommandContext(ctx, r.args[0], r.args[1:]...)
		command.WaitDelay = 5 * time.Second
		command.Stdin, command.Stdout, command.Stderr = in, out, a.diagnostic
		command.Env = append(os.Environ(), "OBERTH_SECRETS_DIR="+dir)
		err := command.Run()
		return errors.Join(ctx.Err(), err)
	}}
	return store.DeliverBroker(ctx, delivery)
}

func projectedStoreToken(path string) ([]byte, error) {
	return readStoreProjection(path, "token-file", 64<<10)
}

// Read bounded CA/token inputs through the kubelet's projection symlinks.
func readStoreProjection(path, flagName string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, storeProjectionProblem(path, flagName, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		clear(data)
		return nil, storeProjectionProblem(path, flagName, err)
	}
	return data, nil
}

func storeProjectionProblem(path, flagName string, cause error) error {
	problem := publicerr.New(publicerr.InvalidInput, "Secret-store input file is missing or invalid.", "Check the named file, its size and its mount, then retry.", cause)
	problem.Context = []string{"Input: --" + flagName, "File: " + path}
	return problem
}
