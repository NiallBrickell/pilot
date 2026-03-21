package main

import (
	"github.com/NiallBrickell/pilot/cmd"
	"os"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
