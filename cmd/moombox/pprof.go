package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
)

// startPprofIfRequested starts an HTTP server on localhost:6060 with the
// runtime/pprof handlers when MOOMBOX_PPROF=1. Diagnostic only — never wired
// when the env var is unset, so production builds aren't exposed.
func startPprofIfRequested() {
	if os.Getenv("MOOMBOX_PPROF") != "1" {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "pprof server panic: %v\n", r)
			}
		}()
		const addr = "localhost:6060"
		fmt.Fprintf(os.Stderr, "pprof listening on http://%s/debug/pprof/\n", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			fmt.Fprintf(os.Stderr, "pprof server error: %v\n", err)
		}
	}()
}
