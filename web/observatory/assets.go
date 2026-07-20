// Package observatory embeds the developer-only stability and run-inspection surface.
package observatory

import "embed"

//go:embed index.html app.css app.js memory.css memory.js brand/*.png
var Assets embed.FS
