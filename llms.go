// Package a12s embeds llms.txt so `a12s init` writes the copy that matches the installed CLI.
package a12s

import _ "embed"

//go:embed llms.txt
var LLMsTxt string
