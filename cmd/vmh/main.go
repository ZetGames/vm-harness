package main

import (
	"os"

	"github.com/ZetGames/vm-harness/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
