// Package web provides embedded static assets for the Moombox web dashboard.
package web

import "embed"

//go:embed public/*
var PublicFS embed.FS
