// Package web carries the built SPA assets that the server embeds.
//
// The directory is currently a placeholder: T7 replaces its contents with the
// real Vite build. FS is embedded into the binary by cmd/dubarr, so the whole
// directory (including any future assets/ subdirectories) ships with it.
package web

import "embed"

// FS is the SPA document root: index.html plus, once T7 lands, the hashed
// asset bundle. Patterns stay directory-based so adding files needs no Go edit.
//
//go:embed *
var FS embed.FS
