// Command logview renders session logs (Claude or Codex) using Bramble's
// OutputModel for unified replay rendering.
package main

import (
	"fmt"
	"os"
)

func main() {
	cfg, err := parseCLIArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, usage(os.Args[0]))
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	hadErrors := false
	for i, path := range cfg.paths {
		rendered, renderErr := renderLog(path, cfg)
		if renderErr != nil {
			hadErrors = true
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, renderErr)
			continue
		}
		if i > 0 {
			fmt.Println()
		}
		fmt.Println(rendered)
	}
	if hadErrors {
		os.Exit(1)
	}
}
