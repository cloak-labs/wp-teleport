package main

import (
	"os"

	"github.com/cloak-labs/wp-teleport/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
