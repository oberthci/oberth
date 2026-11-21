package main

import "golang.org/x/crypto/ssh"

type startupInputs struct {
	settings serverRuntime
	host     ssh.Signer
	identity *serverIdentity
	admin    administrator
}

func resolveStartup(cfg serverConfig, deps serverDependencies) (startupInputs, error) {
	var result startupInputs
	settings, err := decodeRuntime(cfg.Runtime)
	if err != nil {
		return result, err
	}
	if err = validateGit(settings.GitBinary); err != nil {
		return result, err
	}
	result.settings = settings
	result.host, err = readServerSigner(cfg.Files.SSHHostKey, "files.ssh_host_key")
	if err != nil {
		return result, err
	}
	if _, err = readServerSigner(cfg.Files.ForgeKey, "files.forge_key"); err != nil {
		return result, err
	}
	result.identity, result.admin, err = readMounts(cfg, deps)
	return result, err
}
