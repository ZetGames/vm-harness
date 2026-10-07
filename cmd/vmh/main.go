package main

import (
	"os"

	"github.com/fl4metf/vm-harness/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
