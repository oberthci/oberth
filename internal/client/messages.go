package client

import "github.com/oberthci/oberth/internal/publicerr"

func failure(key string, cause error) *publicerr.Error {
	m := catalogue()[key]
	code := publicerr.Unavailable
	if key == "request_size" {
		code = publicerr.InvalidInput
	}
	return publicerr.New(code, m[0], m[1], cause)
}

func catalogue() map[string][2]string {
	return map[string][2]string{
		"request_size":     {"The encoded request exceeds 1048576 bytes.", "Use a smaller request and retry."},
		"client_cert":      {"Server refused this client certificate.", "Run oberth renew if it is still permitted; otherwise ask an administrator for a new invitation."},
		"renewal":          {"Renewal certificate is invalid.", "Keep the current profile and check the server before retrying oberth renew."},
		"bridge_stream":    {"Pushed event stream is unavailable.", "Check server access; use events_wait to read current events."},
		"bridge_close":     {"Could not close the agent session.", "Retry after the idle session expires or ask the administrator to check the server."},
		"bridge_profile":   {"No uplink profile selected for the agent bridge.", "Run oberth join <file>, then use its mcp --profile line or the bound checkout."},
		"bridge_admin":     {"Administrator profiles cannot run the agent bridge.", "Run oberth join <file>, then use its uplink mcp --profile line."},
		"bridge_input":     {"MCP input is invalid or exceeds the request limit.", "Send one bounded JSON-RPC message per line."},
		"bridge_busy":      {"MCP request ID is active or too many requests are open.", "Use a unique request ID and finish an open request before sending another."},
		"bridge_lost":      {"Reply lost; nothing was retried.", "Read the object's state before acting again."},
		"selected":         {"No server selected.", "Pass --url, set OBERTH_URL or run oberth join."},
		"authority":        {"Server certificate is not from the trusted authority.", "Use the installation's CA file with --ca-file."},
		"expired":          {"Server certificate has expired or is not valid yet.", "Check the clock and ask the administrator to renew the server certificate."},
		"hostname":         {"Server certificate is for another name.", "Use the server name in the installation profile."},
		"cancelled":        {"Request interrupted.", "Read the operation's current state when ready."},
		"size":             {"Server reply exceeds the result limit.", "Read a smaller result window."},
		"keys":             {"Cannot generate local client keys.", "Check the local random source and retry."},
		"invitation":       {"Invitation or enrollment certificate is invalid.", "Ask the administrator for a fresh invitation file."},
		"invitation_hosts": {"Invitation file has no SSH host keys.", "Ask the administrator for a new invitation from oberth invite."},
		"invite_hosts":     {"This profile has no SSH host keys to share.", "Run oberth invite with the administrator profile that install saved."},
		"join_unknown":     {"Join did not finish; invitation status is unknown.", "Check the invitation with the administrator before joining again."},
		"join_connect":     {"Cannot reach the invitation server.", "Check the server address and connection; the invitation is still valid."},
		"join_peer":        {"Server certificate does not match the invitation.", "Confirm the server address with the administrator; the invitation is still valid."},
		"profile_ca":       {"CA override differs from the selected profile.", "Use the profile CA; bypass selection with --url, --ca-file and your own --token-file or --client-cert/--client-key."},
		"profile":          {"Client profile is invalid.", "Select an installed profile with OBERTH_PROFILE."},
		"profile_exists":   {"The selected profile already exists or cannot be created.", "Set OBERTH_PROFILE to a new name, then run oberth join again."},
		"http":             {"Plain HTTP is refused; use HTTPS.", "Use an HTTPS URL."},
		"url":              {"Server address is invalid or insecure.", "Use an HTTPS URL."},
		"trust":            {"Client TLS identity or trust is invalid.", "Check the CA file and matching client certificate and key."},
		"no_reply":         {"Server is reachable but not answering.", "Check server diagnostics, then read the operation again."},
		"connect":          {"Cannot reach the server.", "Check the server address and connection, then retry."},
		"local_dns":        {"Cannot resolve this .local server name.", "Use an IP address or a DNS name this client can resolve, then rerun the command."},
		"reply":            {"Server reply is invalid.", "Check the server address and version, then read the operation again."},
		"redirect":         {"Server redirect refused.", "Use the installation's original server address."},
		"deadline":         {"Request timed out.", "Check the connection and read the operation's current state."},
	}
}

func fileMessages() map[string][2]string {
	return map[string][2]string{
		"write":     {"Cannot save the private client file.", "Use an owned destination directory with mode 700."},
		"unsafe":    {"Client file is not safe to use.", "Use an owned regular file; remove group and other write permissions."},
		"private":   {"Client file is not private.", "Run chmod 600 on the file shown below."},
		"missing":   {"Client file was not found.", "Check the file path or select an installed profile."},
		"directory": {"Client directory is not private or owned by this user.", "Use an owned directory and run chmod 700 on the path shown below."},
		"pair":      {"Client certificate and key do not form a usable pair.", "Supply both --client-cert and its matching --client-key."},
		"trust":     {"CA file contains no usable certificates.", "Select the installation's CA certificate file."},
		"profile":   {"Client profile metadata is invalid.", "Select an installed profile with OBERTH_PROFILE or run oberth join."},
	}
}
