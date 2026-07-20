// Package conversation embeds Eri's primary single-conversation surface.
package conversation

import "embed"

//go:embed index.html app.css app.js observation.css observation.js manifest.webmanifest brand/*.png
var Assets embed.FS
