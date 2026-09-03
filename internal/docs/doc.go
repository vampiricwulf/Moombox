// Package docs carries no production code. It exists so the spec-citation
// test beside it lives in a package `go test ./...` reaches, and so
// `go build ./...` sees an ordinary package rather than a directory of
// nothing but test files. The checks are in citations_test.go.
package docs
