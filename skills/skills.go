// Package skills embeds the agent guide so `horch guide` always matches the binary.
package skills

import _ "embed"

//go:embed horch/SKILL.md
var Horch string
