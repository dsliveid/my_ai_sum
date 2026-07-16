package web

import "embed"

// StaticFiles contains the browser UI served by the backend.
//
//go:embed static/*
var StaticFiles embed.FS
