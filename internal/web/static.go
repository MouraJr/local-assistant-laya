package web

import "embed"

// staticFS holds the built CSS plus the vendored htmx and Alpine bundles.
// Embedding them keeps the promise that the binary is self-contained and works
// with no network and no files beside it.
//
//go:embed all:static
var staticFS embed.FS
