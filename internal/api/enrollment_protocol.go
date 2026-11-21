package api

func redemptionDescription(name string) string {
	switch name {
	case "secret":
		return "Supply the invitation secret through this authenticated request body."
	case "server_url":
		return "Confirm the exact public server URL supplied by the invitation."
	case "ca_pin":
		return "Confirm the invitation certificate authority pin before installing credentials."
	case "csr":
		return "Supply the signing request created with your locally retained private key."
	case "ssh_public_key":
		return "Supply only the public half of your locally generated SSH key."
	}
	return "Supply the invitation value for this redemption."
}
