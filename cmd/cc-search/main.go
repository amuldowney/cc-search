package main

import (
	"os"

	"github.com/amuldowney/cc-search/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Config{StartRefresh: cli.StartBackgroundRefresh}, os.Stdout, os.Stderr))
}
