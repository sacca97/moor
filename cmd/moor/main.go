// Command moor runs interactive shells in persistent PTYs that can be
// detached with Ctrl-\ twice (or Ctrl-b d) and reattached later.
package main

import (
	"os"
	"runtime/debug"

	"github.com/sacca/moor/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cli.Version = version
	if version == "dev" {
		// Built with "go install": use the module version, if there is one.
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			cli.Version = bi.Main.Version
		}
	}
	os.Exit(cli.Main(os.Args[1:]))
}
