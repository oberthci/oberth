package api

import "github.com/oberthci/oberth/internal/publicerr"

type message struct {
	code           publicerr.Code
	reason, remedy string
}

func (m message) err() *publicerr.Error { return m.caused(nil) }

func (m message) caused(cause error) *publicerr.Error {
	return publicerr.New(m.code, m.reason, m.remedy, cause)
}

// messages is the complete transport refusal catalogue, constructed per owner.
func messages() map[string]message {
	return map[string]message{
		"originConfig":       {publicerr.InvalidInput, "MCP browser origin is invalid.", "Configure the server's public HTTP or HTTPS URL."},
		"mcpMethod":          {publicerr.InvalidInput, "Only POST is supported for MCP.", "Use MCP 2026-07-28 through the pinned stdio bridge or CLI."},
		"origin":             {publicerr.Forbidden, "MCP browser origin is not allowed.", "Use the configured server URL or the pinned stdio bridge."},
		"skill":              {publicerr.InvalidInput, "Skill or embedded file is unavailable.", "Rediscover skills/list, approve the current origin and manifest, then reload the skill."},
		"headers":            {publicerr.InvalidInput, "MCP 2026-07-28 headers do not match.", "Use MCP 2026-07-28 with matching version, method and name headers."},
		"version":            {publicerr.InvalidInput, "MCP protocol version is unsupported.", "Use MCP 2026-07-28 and rediscover the server."},
		"metadata":           {publicerr.InvalidInput, "MCP request metadata is invalid.", "Send protocolVersion and clientCapabilities in each request metadata object."},
		"bodySize":           {publicerr.InvalidInput, "Request body exceeds this route's byte limit.", "Send fewer arguments or a smaller input body."},
		"bodyRead":           {publicerr.InvalidInput, "Request body could not be read completely.", "Reconnect and resend the complete JSON body."},
		"invitation":         {publicerr.Forbidden, "Invitation redemption was refused.", "Check the invitation and local key material, or request a new invitation."},
		"deadline":           {publicerr.Unavailable, "Observation timed out.", "Read the current state before retrying the observation."},
		"cancelled":          {publicerr.Unavailable, "Request interrupted.", "Read the operation's current state when ready."},
		"actionTimeout":      {publicerr.Unavailable, "Action timed out; it may have completed.", "Inspect the read tool named in detail before repeating this action."},
		"input":              {publicerr.InvalidInput, "Invalid request arguments.", "Use the declared fields and limits for this operation."},
		"registry":           {publicerr.Invariant, "Tool registry is incomplete or inconsistent.", "Bind every declared tool once with its typed handler and classification."},
		"auth":               {publicerr.Forbidden, "Caller credentials are invalid.", "Reconnect with a current bearer or verified client certificate."},
		"authbusy":           {publicerr.Unavailable, "Authentication is temporarily unavailable.", "Retry in one second."},
		"forbidden":          {publicerr.Forbidden, "This operation requires more authority.", "Use an identity authorized for this operation."},
		"unknown":            {publicerr.NotFound, "Tool or route is unknown.", "List the available tools or use a documented route."},
		"longPoll":           {publicerr.Unavailable, "This identity has reached its long-poll limit.", "Cancel an unused long poll, or wait for it to finish before retrying."},
		"nativeIdentity":     {publicerr.Unavailable, "This identity has reached its native log stream limit.", "Close an unused native log stream, then retry."},
		"nativeInstallation": {publicerr.Unavailable, "This installation has reached its native log stream limit.", "Close an unused native log stream, then retry."},
		"restarting":         {publicerr.Unavailable, "Server is restarting.", "Reconnect and inspect durable state before repeating an action."},
		"internal":           {publicerr.Invariant, "Operation failed.", "Use the correlation reference to inspect server diagnostics."},

		"output": {publicerr.Unavailable, "Response exceeds its byte limit.", "Request a smaller page or a narrower log window."},
		"media":  {publicerr.InvalidInput, "This route requires a JSON body without query arguments.", "Send application/json with all arguments in its body."},
	}
}

func refusal(key string) *publicerr.Error { return messages()[key].err() }
