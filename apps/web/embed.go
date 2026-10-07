// Package web holds the browser task manager served by `todo serve`: plain
// HTML, CSS and ES modules with no build step and no runtime dependencies.
// The files are embedded into the todo binary, so the page always matches
// the API of the server that serves it.
package web

import "embed"

// Assets is the static site; index.html is the entry page.
//
//go:embed index.html styles.css js/*.js
var Assets embed.FS
