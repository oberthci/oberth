package sshserver

import (
	"github.com/oberthci/oberth/internal/reponame"
	"strings"
)

const reasonScope = "Git key cannot access this repository"
const remedyScope = "Register this key for the requested repository."

const reasonTemporary = "temporary Git identity lookup failure"
const reasonUnregistered = "Git key is unregistered"
const remedyRegister = "Register the SSH key and its repository scope."
const reasonFailed = "Git request failed"
const remedyRetry = "Retry the push."
const reasonConfig = "Git server configuration is invalid"
const remedyConfig = "Configure the host key, identity lookup and Git actions."
const remedyCommand = "Use git-upload-pack or git-receive-pack with a canonical repository."
const reasonCommand = "invalid Git service command"
const reasonIdentity = "Git identity is unavailable"

type Command struct{ Service, Repository string }

func ParseCommand(value string) (Command, error) {
	service, repo, ok := strings.Cut(value, " ")
	if !ok || service != "git-upload-pack" && service != "git-receive-pack" {
		return Command{}, failure(reasonCommand)
	}
	if len(repo) >= 2 && strings.HasPrefix(repo, "'") && strings.HasSuffix(repo, "'") {
		repo = repo[1 : len(repo)-1]
	}
	repo = strings.TrimPrefix(repo, "/")
	repo = strings.TrimSuffix(repo, ".git")
	if _, err := reponame.ParseRepository(repo); err != nil {
		return Command{}, failure(reasonCommand)
	}
	return Command{Service: service, Repository: repo}, nil
}
