package apicontract

import "context"

const SecretBodyBytes = 2 << 20

// SecretPutArgs names public authority only. Secret bytes are never tool arguments.
type SecretPutArgs struct {
	Repo     string `sensitive:"false" position:"0" json:"repo" required:"true" format:"repo" desc:"Select the repository owning this exact secret grant."`
	Workflow string `sensitive:"false" position:"0" json:"workflow" required:"true" desc:"Select the authored file owning the approved secret grant."`
	Step     string `sensitive:"false" position:"0" json:"step" required:"true" desc:"Select the exact step owning the approved secret grant."`
	Secret   string `sensitive:"false" position:"1" json:"secret" required:"true" desc:"Select the relative path within this step account."`
}

// SecretPutBody is the private POST /api/secrets/put body. Value is base64 JSON.
// It is not part of the tool argument schema or any result.
type SecretPutBody struct {
	SecretPutArgs
	Value []byte `json:"value"`
}

type secretValueKey struct{}

// WithSecretValue binds a private request body for the lifetime of its call.
// The transport clears value after the call completes.
func WithSecretValue(ctx context.Context, value []byte) context.Context {
	return context.WithValue(ctx, secretValueKey{}, value)
}

func SecretValue(ctx context.Context) ([]byte, bool) {
	value, ok := ctx.Value(secretValueKey{}).([]byte)
	return value, ok
}
