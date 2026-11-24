package main

import (
	"os"

	"github.com/SuperMarioYL/tryfirst/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
