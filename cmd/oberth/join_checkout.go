package main

import (
	"encoding/json"
	"errors"
	"strconv"

	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/client"
)

func (s *flowSession) bindJoinedCheckout(dir string) (bool, error) {
	repo, err := s.git("config", "--get", "oberth.repository")
	if err != nil || repo == "" {
		return false, nil
	}
	profile, known, err := s.joinedConnection(dir)
	if err != nil {
		return false, err
	}
	parsed, err := profileSSH(profile, profile.ServerURL)
	if err != nil {
		return false, err
	}
	if err = checkCheckoutTrust(known, parsed); err != nil {
		return false, err
	}
	if err = s.checkGitBinding(repo, "join"); err != nil {
		return false, err
	}
	command, err := s.pushCommand(dir, known, parsed, filepath.Join(dir, client.UplinkPushKeyFile))
	if err != nil {
		return false, err
	}
	if err = s.bindGitRemote(repo, command); err != nil {
		return false, err
	}
	if _, err = s.git("config", "oberth.profile", dir); err != nil {
		return false, err
	}
	_, err = s.git("config", "oberth.uplinkProfile", filepath.Base(dir))
	return true, err
}

func joinProfileName(base, host, selected string) string {
	if selected != "" {
		return selected
	}
	name := host
	for i := 1; ; i++ {
		if _, err := os.Lstat(filepath.Join(base, name)); errors.Is(err, os.ErrNotExist) {
			return name
		} else if err != nil {
			return name
		}
		name = host + "-uplink"
		if i > 1 {
			name += "-" + strconv.Itoa(i)
		}
	}
}

func (s *flowSession) joinedConnection(dir string) (client.Profile, string, error) {
	var profile client.Profile
	raw, err := client.ReadPublic(filepath.Join(dir, "installation.json"))
	if err != nil {
		return profile, "", err
	}
	if json.Unmarshal(raw, &profile) != nil {
		return profile, "", flowError("input", nil)
	}
	// The invitation's keys pin the server; a checkout's own settings never do.
	known := profile.KnownHosts
	if known == "" {
		return profile, "", flowError("host_unsaved", nil)
	}
	return profile, known, nil
}
