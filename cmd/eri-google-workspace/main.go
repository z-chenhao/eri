package main

import (
	"context"
	"fmt"
	"os"

	"github.com/z-chenhao/eri/plugins/googleworkspace"
)

func main() {
	if err := googleworkspace.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "eri-google-workspace:", err)
		os.Exit(1)
	}
}
