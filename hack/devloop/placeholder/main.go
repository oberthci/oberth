// Command placeholder serves the development readiness endpoint.
// Spec: hack/README.md#development-loop
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	server := &http.Server{
		Addr:              ":8080",
		Handler:           healthHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
