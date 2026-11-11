package model

import "context"

type authMethodKey struct{}

func WithAuthMethod(ctx context.Context, method string) context.Context {
	return context.WithValue(ctx, authMethodKey{}, method)
}
func AuthMethod(ctx context.Context) string {
	value, _ := ctx.Value(authMethodKey{}).(string)
	return value
}
