//go:build !linux

package secretstore

func memoryDirectory(string) error { return refuse("delivery", nil) }
