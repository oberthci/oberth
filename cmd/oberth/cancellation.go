package main

import (
	"context"
	"errors"
)

func cancellationOnly(err error) bool {
	if err == nil {
		return false
	}
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !cancellationOnly(child) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, context.Canceled)
}
