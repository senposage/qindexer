package web

import "embed"

// Static contains the embedded admin web UI.
//
//go:embed static/*
var Static embed.FS
