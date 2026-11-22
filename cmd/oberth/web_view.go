package main

// webBearerCommand is the administrator command that writes a browser-only
// sign-in bearer into a new private file.
const webBearerCommand = "oberth uplink add --identity browser --bearer-file <new file>"

// installWebView tells the administrator how to open the web view: the browser
// trusts the installation's CA, printed above as Server CA, and signs in with a
// bearer.
const installWebView = "Web view: trust Server CA and sign in with a bearer from " + webBearerCommand + "."

// joinedWebView names the joined profile's CA file; the bearer comes from an
// administrator.
func joinedWebView(ca string) string {
	return "Web view: trust " + ca + "; an administrator makes your bearer with " + webBearerCommand + "."
}
