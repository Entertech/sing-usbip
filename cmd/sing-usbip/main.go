//go:build linux || (darwin && cgo) || windows

package main

import (
	"errors"
	"os"

	"github.com/sagernet/sing-box/log"
)

func main() {
	err := mainCommand.Execute()
	var exit exitError
	if errors.As(err, &exit) {
		log.Error(err)
		os.Exit(exit.code)
	}
	if err != nil {
		log.Fatal(err)
	}
}
