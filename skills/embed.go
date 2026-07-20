// Package skills embeds Eri's standard Agent Skills for single-binary delivery.
package skills

import "embed"

// Builtin contains directories whose entrypoint is SKILL.md. The packages are
// ordinary Agent Skills and can also be copied into another compatible client.
//
//go:embed all:*
var Builtin embed.FS
