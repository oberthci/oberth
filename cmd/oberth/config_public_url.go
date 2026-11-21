package main

import (
	"net/url"

	"github.com/oberthci/oberth/internal/apicontract"
)

func publicURLHost(address string) error {
	u, _ := url.Parse(address)
	if apicontract.ASCIIHost(u.Hostname()) {
		return nil
	}
	ascii, err := apicontract.PunycodeAddress(u)
	if err != nil {
		return serverProblem("config", "public.url: host has no valid punycode form", nil)
	}
	problem := serverProblem("public-host", "public.url: host must use ASCII", nil)
	problem.Context = []string{"public.url: " + ascii}
	return problem
}
