package main

import (
	"encoding/json"
	"errors"
	"strconv"

	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/publicerr"
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
		return false, joinTrustRefusal(err)
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
	return profile, profile.KnownHosts, nil
}

// joinTrustRemedy is what join can do about the server's SSH host keys: it
// takes no file of its own, only an invitation that carries them.
const joinTrustRemedy = "Ask the administrator for a new invitation and join with it."

// joinTrustRefusal keeps a trust refusal's reason and file and replaces the
// repo add remedy, which names a flag join does not have.
func joinTrustRefusal(err error) error {
	var problem *publicerr.Error
	if !errors.As(err, &problem) || problem.Remedy() != serverTrustRemedy {
		return err
	}
	refusal := publicerr.New(problem.Code(), problem.Reason(), joinTrustRemedy, err)
	refusal.ID, refusal.Context = problem.ID, problem.Context
	return refusal
}
