package main

import (
	"fmt"
	"os"

	"github.com/mward-msft1/Version_2_GCP/internal/auth"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: hostedcache <tokens.json>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not read token cache")
		os.Exit(1)
	}
	filtered, err := auth.HostedCache(string(raw))
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	fmt.Print(filtered)
}
