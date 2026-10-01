package main

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/service"
)

func TestAnInvalidLogPatternIsClientErrorNotServerError(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("read bounded run log: %w",
		fmt.Errorf("%w: missing closing )", runlog.ErrInvalidPattern))

	code, message := classifyViewError(wrapped)
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: a caller's bad pattern is not a server fault", code)
	}
	if message == "internal error" {
		t.Fatal("the compile failure was replaced by a generic message")
	}
}

func TestUnclassifiedErrorsStayGeneric(t *testing.T) {
	t.Parallel()
	code, message := classifyViewError(errors.New("something internal"))
	if code != http.StatusInternalServerError || message != "internal error" {
		t.Fatalf("code = %d message = %q, want a generic 500", code, message)
	}
}

func TestInvalidInputRemainsClientError(t *testing.T) {
	t.Parallel()
	code, _ := classifyViewError(fmt.Errorf("%w: step is required", service.ErrInvalidInput))
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", code)
	}
}
