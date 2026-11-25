package secretstore

func refusal(code string) (string, string) {
	if code == "broker" {
		return "Secret delivery is unavailable.", "Read the run diagnosis, restore the unavailable service, then rerun."
	}
	return storeRefusal(code)
}

func storeRefusal(code string) (string, string) {
	switch code {
	case "address":
		return "Secret store address is invalid.", "Use an HTTPS origin without credentials, path, query or fragment."
	case "trust":
		return "Secret store trust is missing.", "Supply its trusted CA and certificate server name."
	case "connect":
		return "Secret store connection failed.", "Check the store address, certificate and readiness, then retry."
	case "sealed":
		return "Secret store is sealed.", "An administrator runs oberth unseal, then retry."
	case "request":
		return "Secret store refused the request.", "Check the configured role, account, policy and store state."
	case "response":
		return "Secret store returned an invalid response.", "Use the supported store API and retry with a complete response."
	case "limit":
		return "Secret data exceeds its bound or is incomplete.", "Use values at most 1 MiB and complete store responses at most 2 MiB."
	case "drift":
		return "Secret store configuration differs from the reviewed policy.", "Inspect the named configuration and reconcile it explicitly before setup."
	default:
		return operationRefusal(code)
	}
}

func operationRefusal(code string) (string, string) {
	switch code {
	case "backup-setup":
		return "Store backup authorization did not finish.", "Unseal the bundled store, inspect native authorization progress, then retry this explicit backup setup."
	case "backup-root-disabled":
		return "Store root generation is disabled.", "Set disable_unauthed_generate_root_endpoints = false on the store listener, restart, unseal, then retry."
	case "token":
		return "The projected account token is invalid.", "Use the Pod's readable service-account token file."
	case "delivery":
		return "Secret delivery could not be completed.", "Use a new private directory; no partial secret set is published."
	case "path":
		return "A secret store path is invalid.", "Use relative path segments containing letters, digits, dots, underscores or hyphens."
	default:
		return "Secret store input is incomplete or invalid.", "Supply explicit store mounts, roles, namespaces and account bindings."
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
