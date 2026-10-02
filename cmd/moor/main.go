package main

import (
	"os"

	"github.com/sacca/moor/internal/cli"
)

var version = "dev"

func run(args []string) int {
	cli.Version = version
	return cli.Main(args)
}

func main() {
	os.Exit(run(os.Args[1:]))
}
