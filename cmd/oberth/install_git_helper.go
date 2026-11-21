package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/installer"
)

// repointGit runs repo add's repair of the profile's Git helper, so the helper
// names the saved key and this client, and returns the files the helper names.
// A profile no checkout uses has no helper.
func (s installationSession) repointGit(ctx context.Context, p installer.Profile) ([]string, bool) {
	dir := filepath.Dir(p.AdminKey)
	config := filepath.Join(dir, "ssh.config")
	if _, err := os.Lstat(config); errors.Is(err, fs.ErrNotExist) {
		return nil, true
	}
	known, err := helperKnownHosts(config, p.SSHURL)
	if err == nil {
		session := &flowSession{app: s.app, ctx: ctx, resolved: client.Options{URL: p.ServerURL, ClientKey: p.AdminKey}}
		_, _, err = session.checkoutConnection(known)
	}
	if err == nil {
		return []string{p.AdminKey, known}, true
	}
	if s.app.diagnostic != nil {
		message := "Git helper not repaired.\nIn one checkout of this profile, run oberth repo add <forge URL>."
		if earlier, _ := installer.EarlierCredentials(p); len(earlier) > 0 {
			message += "\nEarlier credential files are kept in " + dir + "."
		}
		// One line per sentence, so the directory stays whole and copyable.
		_, _ = fmt.Fprintln(s.app.diagnostic, message)
	}
	return nil, false
}

func helperKnownHosts(config, sshURL string) (string, error) {
	endpoint, err := url.Parse(sshURL)
	if err != nil {
		return "", err
	}
	helper, err := gitssh.Configure(config, gitssh.Config{Host: endpoint.Hostname()})
	return helper.KnownHosts, err
}
