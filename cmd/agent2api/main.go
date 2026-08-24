// Command agent2api serves local agent CLIs as OpenAI- and Anthropic-compatible
// HTTP APIs.
package main

import (
	"fmt"
	"os"

	// Linking an adapter package in is what enables that backend.
	_ "github.com/Dongss/agent2api/internal/adapter/claudecode"
	_ "github.com/Dongss/agent2api/internal/adapter/codex"
	_ "github.com/Dongss/agent2api/internal/adapter/cursor"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "agent2api: "+err.Error())
		os.Exit(1)
	}
}
