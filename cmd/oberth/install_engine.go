package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/kubeconfig"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/setuptui"
	"github.com/oberthci/oberth/internal/stateport"
	definitionclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
)

type installationSession struct {
	app               application
	request           installationRequest
	engine            *installer.Engine
	choices           installer.Choices
	profile           installer.Profile
	name              string
	options           stateport.Options
	input             *bufio.Scanner
	interactivePrompt bool
}

func (a application) runInstaller(ctx context.Context, r installationRequest, in io.Reader, out io.Writer) error {
	if r.name == "secretstore setup" || r.name == "secretstore check" {
		return runStoreOperator(ctx, r, out)
	}
	if r.name == "secret put" {
		return a.runSecretPut(ctx, r, in, out)
	}
	session, err := a.openInstallation(r)
	if err != nil {
		return err
	}
	if r.name == "backup" {
		return session.backup(ctx, out)
	}
	if r.enabled("verify") {
		if err := session.verify(ctx); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "Verified: "+safeDisplay(session.profile.ServerURL))
		return err
	}
	stopInput := watchInstallationInput(ctx, in)
	defer stopInput()
	if err = session.prompt(ctx, in, out); err != nil {
		return errors.Join(ctx.Err(), err)
	}
	if err = session.openEngine(); err != nil {
		return err
	}
	defer session.engine.Credentials.Close()
	return session.dispatch(ctx, in, out)
}

func (a application) openInstallation(r installationRequest) (installationSession, error) {
	s := installationSession{app: a, request: r, choices: r.choices(), name: installationProfileName(r)}
	var err error
	s.options, err = installationStateOptions(r)
	if err != nil {
		return s, err
	}
	profile, dir, err := a.selectedInstallation(r)
	if err != nil {
		return s, err
	}
	if profile.Uplink {
		return s, installationProblem("An uplink profile cannot own an installation.", "Choose an administrator installation profile with --profile.")
	}
	if dir != "" {
		s.profile, err = readInstallationProfile(dir, r.name == "install")
		if err != nil {
			return s, err
		}
	}
	if s.choices.Context == "" {
		s.choices.Context = s.profile.Context
	}
	if s.name == "" && dir != "" {
		s.name = filepath.Base(dir)
	}
	if s.choices.Namespace == "" {
		s.choices.Namespace = s.profile.Namespace
	}
	return s, nil
}

func installationProfileName(r installationRequest) string {
	if r.name == "install" && r.text("profile") == "" {
		return defaultInstallationProfile(r.text("context"), r.text("namespace"))
	}
	return r.text("profile")
}

func (a application) selectedInstallation(r installationRequest) (client.Profile, string, error) {
	profile, dir, err := client.SelectedProfile(a.base, installationProfileName(r))
	if r.name == "install" && errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return profile, dir, err
}

func readInstallationProfile(dir string, fresh bool) (installer.Profile, error) {
	var profile installer.Profile
	data, err := client.ReadPublic(filepath.Join(dir, "installation.json"))
	if fresh && errors.Is(err, fs.ErrNotExist) {
		return profile, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &profile)
	}
	return profile, err
}

func (s *installationSession) openEngine() error {
	if s.choices.Context == "" {
		return installationProblem("Select an explicit kubeconfig context.", "Pass --context when installing, or select an installed administrator profile.")
	}
	if s.app.diagnostic != nil {
		if err := writeWrapped(s.app.diagnostic, "Checking the selected installation and release."); err != nil {
			return err
		}
	}
	target, err := kubeconfig.Open(s.app.env("KUBECONFIG"), s.choices.Context)
	if err != nil {
		return installer.NewProblem("cluster", err, "Context: "+s.choices.Context)
	}
	dir, err := client.ProfileDir(s.app.base, s.name)
	if err != nil {
		return err
	}
	credentials, err := installer.OpenConfigDir(dir)
	if err != nil {
		return err
	}
	definitions, err := definitionclient.NewForConfig(target.Config)
	if err != nil {
		_ = credentials.Close()
		return err
	}
	s.engine = &installer.Engine{Definitions: definitions.ApiextensionsV1().CustomResourceDefinitions(), Cluster: target, Charts: installer.NewChartLibrary(target, 5*time.Minute), Credentials: credentials, Random: rand.Reader, Now: s.app.config.Time, CheckEndpoint: installer.Verify, GitHelper: s.repointGit}
	return nil
}

func (s installationSession) dispatch(ctx context.Context, in io.Reader, out io.Writer) error {
	switch s.request.name {
	case "unseal":
		status, err := s.engine.Unseal(ctx)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, status)
		return err
	case "rollback":
		if s.request.enabled("dry-run") || !s.request.enabled("yes") {
			return s.reviewRollback(out)
		}
		return s.rollback(ctx, out)
	case "upgrade":
		return s.changeInstallation(ctx, in, out)
	case "install":
		return s.applyInstallation(ctx, in, out)
	default:
		return usageProblems{"Unknown installation command."}
	}
}

func (s installationSession) applyInstallation(ctx context.Context, in io.Reader, out io.Writer) error {
	release, err := installationRelease(ctx, s.request)
	if err != nil {
		return err
	}
	plan, err := s.prepareReachableInstallation(ctx, release, out)
	if err != nil {
		return err
	}
	defer plan.Close()
	approved, err := installationApproval(ctx, installationReview{request: s.request, plan: plan, detail: plan.Detail(), scanner: s.input}, in, out)
	if err != nil || approved == "" {
		return err
	}
	selection, err := client.PrepareProfileSelection(s.app.base, s.name)
	if err != nil {
		return err
	}
	defer selection.Close()
	result, err := s.engine.Apply(ctx, plan, approved, setuptui.Progress(out))
	if err != nil {
		return err
	}
	if err = selection.Commit(); err != nil {
		return err
	}
	if err = showInstallation(out, result); err != nil {
		return err
	}
	app := s.app
	app.out = out
	return app.agentTools("")
}

type installationReview struct {
	request installationRequest
	plan    *installer.Plan
	detail  []string
	scanner *bufio.Scanner
}

func installationApproval(ctx context.Context, review installationReview, in io.Reader, out io.Writer) (string, error) {
	if review.request.text("output") == "json" {
		return "", json.NewEncoder(out).Encode(review.plan)
	}
	if review.request.enabled("detail") {
		for _, line := range review.detail {
			if err := writeWrapped(out, safeDisplay(line)); err != nil {
				return "", err
			}
		}
	}
	if review.request.enabled("dry-run") || review.request.enabled("yes") {
		for _, line := range review.plan.Review() {
			if err := writeWrapped(out, safeDisplay(line)); err != nil {
				return "", err
			}
		}
		if review.request.enabled("dry-run") {
			return "", nil
		}
		return review.plan.Digest(), ctx.Err()
	}
	scanner := review.scanner
	if scanner == nil {
		scanner = bufio.NewScanner(in)
	}
	return setuptui.Review(ctx, reviewTerminal(in, scanner, out), review.plan)
}
func showInstallation(out io.Writer, result installer.Result) error {
	for _, line := range installationFacts(result) {
		if line != "" {
			if err := writeWrapped(out, safeDisplay(line)); err != nil {
				return err
			}
		}
	}
	return nil
}

func installationFacts(result installer.Result) []string {
	backup := ""
	if result.BackupPath != "" {
		backup = "Pre-upgrade backup: " + result.BackupPath
	}
	profile := result.Profile
	return []string{"Server: " + profile.ServerURL, "Profile: " + filepath.Join(filepath.Dir(profile.AdminCert), "installation.json"),
		"Administrator certificate: " + profile.AdminCert, "Administrator key: " + profile.AdminKey,
		"Server CA: " + profile.CAFile, "SSH trust: " + profile.KnownHosts,
		"SSH host fingerprint: " + result.SSHHostFingerprint, "Deploy public key: " + result.DeployPublicKey,
		result.Next, installWebView, result.StoreSnapshotHint, backup, result.BackupReminder}
}

func installationOutput(r installationRequest) error {
	format := r.text("output")
	if format != "" && format != "text" && format != "json" {
		return usageProblems{"--output must be text or json."}
	}
	if format == "json" && r.name != "backup" && !r.enabled("dry-run") {
		return usageProblems{"--output json requires --dry-run for an installation review."}
	}
	return nil
}
func installationProblem(reason, remedy string) *publicerr.Error {
	return publicerr.New(publicerr.InvalidInput, reason, remedy, nil)
}

func defaultInstallationProfile(context, namespace string) string {
	if context != "" && namespace != "" && namespace != installer.Defaults().Namespace {
		return context + "-" + namespace
	}
	return context
}
