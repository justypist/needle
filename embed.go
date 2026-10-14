// Package needle exists so the dashboard assets can be embedded from the
// repository root, where they are easy to find and edit.
package needle

import "embed"

//go:embed web
var WebFS embed.FS
