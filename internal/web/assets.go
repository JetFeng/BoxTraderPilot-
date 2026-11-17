// Package web assets are embedded into the binary so the server has no
// runtime file dependencies and needs no front-end build step.
package web

import "embed"

//go:embed assets
var embeddedAssets embed.FS
