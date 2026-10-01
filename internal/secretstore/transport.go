package secretstore

import "fmt"

// requireVerifiedHTTPS is the fail-before-login gate for operations whose
// payload must never cross the development-only HTTP transport. HTTPS clients
// always verify against either the configured CA pool or the system roots;
// this package never enables InsecureSkipVerify.
func (client *Client) requireVerifiedHTTPS(operation string) error {
	if client == nil || !client.verifiedHTTPS {
		return fmt.Errorf("secret store %s requires verified HTTPS transport", operation)
	}
	return nil
}
