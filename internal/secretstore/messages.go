package secretstore

import "github.com/oberthci/oberth/internal/runtimeconfig"

func refusal(code string) (string, string) {
	catalogue := refusalCatalogue()
	message, ok := catalogue[code]
	if !ok {
		message = catalogue["input"]
	}
	return message[0], message[1]
}

func refusalCatalogue() map[string][2]string {
	reason, remedy := runtimeconfig.SecretStoreConfiguration()
	return map[string][2]string{
		"configuration":        {reason, remedy},
		"broker":               {"Secret delivery is unavailable.", "Read the run diagnosis, restore the unavailable service, then rerun."},
		"address":              {"Secret store address is invalid.", "Use an HTTPS origin without credentials, path, query or fragment."},
		"trust":                {"Secret store trust is missing.", "Supply its trusted CA and certificate server name."},
		"connect":              {"Secret store connection failed.", "Check the store address, certificate and readiness, then retry."},
		"sealed":               {"Secret store is sealed.", "An administrator runs oberth unseal, then retry."},
		"request":              {"Secret store refused the request.", "Check the configured role, account, policy and store state."},
		"response":             {"Secret store returned an invalid response.", "Use the supported store API and retry with a complete response."},
		"limit":                {"Secret data exceeds its bound or is incomplete.", "Use values at most 1 MiB and complete store responses at most 2 MiB."},
		"drift":                {"Secret store configuration differs from the reviewed policy.", "Inspect the named configuration and reconcile it explicitly before setup."},
		"backup-unfinished":    {"Store root generation is unfinished.", "Cancel it with DELETE sys/generate-root/attempt, check the saved share, then retry setup."},
		"backup-pending":       {"Store root authorization needs cleanup.", "Follow docs/install.md#back-up to revoke root accessors and cancel generation; remove the pending receipt, then retry."},
		"backup-receipt":       {"Authorization receipt cannot be saved.", "Copy --config into a writable persistent directory; keep its .backup-setup.json receipt until setup confirms revocation."},
		"backup-setup":         {"Store backup authorization did not finish.", "Unseal the bundled store, inspect native authorization progress, then retry this explicit backup setup."},
		"backup-root-disabled": {"Store root generation is disabled.", "Set disable_unauthed_generate_root_endpoints = false on the store listener, restart, unseal, then retry."},
		"token":                {"The projected account token is invalid.", "Use the Pod's readable service-account token file."},
		"delivery":             {"Secret delivery could not be completed.", "Use a new private directory; no partial secret set is published."},
		"path":                 {"A secret store path is invalid.", "Use relative path segments containing letters, digits, dots, underscores or hyphens."},
		"input":                {"Secret store input is incomplete or invalid.", "Supply explicit store mounts, roles, namespaces and account bindings."},
	}
}

func fileRefusal(field, state string) (string, string) {
	switch state {
	case "absolute":
		return field + " must be an absolute file path.", "Set this field to an absolute file path."
	case "missing":
		return field + " is missing.", "Mount the configured file, then retry."
	case "regular":
		return field + " is not a regular file.", "Mount a regular file at the configured path."
	case "empty":
		return field + " is empty.", "Provide a non-empty regular file at the configured path."
	case "large":
		return field + " is larger than its 64 KiB limit.", "Use a file of at most 64 KiB."
	default:
		return field + " cannot be read.", "Make the configured file readable by the server."
	}
}
