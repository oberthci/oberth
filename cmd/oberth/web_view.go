package main

import "encoding/json"

// webBearerCommand is the Browser access guide's command for a sign-in bearer:
// an unused identity name, the given upstream scope, one hour, a new private
// file.
func webBearerCommand(scope string) string {
	return "oberth uplink add --identity <unused-name> --scope " + shellArgument(scope) + " --ttl 1h --bearer-file <new-file>"
}

// installWebView tells the administrator how to open the web view: the browser
// trusts the installation's CA, printed above as Server CA, and signs in with a
// bearer the command on the next line writes.
func installWebView() string {
	return "Web view: trust Server CA and sign in with a bearer this command writes:\n" + webBearerCommand(`["<upstream>"]`)
}

// joinedWebView names the joined profile's CA file; an administrator writes the
// bearer for the joined identity's own scope.
func joinedWebView(ca string, scope []string) string {
	raw, _ := json.Marshal(append([]string{}, scope...))
	return "Web view: trust " + ca + " and sign in with a bearer an administrator writes with:\n" + webBearerCommand(string(raw))
}
