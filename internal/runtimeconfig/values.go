package runtimeconfig

import "encoding/json"

type WorkflowDefaultsSnapshot struct {
	WorkflowDefaults json.RawMessage `json:"workflow_defaults"`
	Digest           string          `json:"digest"`
}

type SourcePluginConfig struct {
	Endpoint    string `json:"endpoint"`
	ServerName  string `json:"server_name"`
	CAPEM       string `json:"ca_pem"`
	ImageDigest string `json:"image_digest"`
}
