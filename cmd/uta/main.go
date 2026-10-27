// Command uta is the CLI entrypoint. All behavior lives under internal/cli
// and the packages it pulls in; this file only wires main() to Execute().
package main

import (
	"github.com/unleashtheagents/uta/internal/cli"
	"github.com/unleashtheagents/uta/internal/paths"
)

func main() {
	paths.LoadCredentials()
	cli.Execute()
}
