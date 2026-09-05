// Command rolemux is the thin host-facing CLI for durable, role-aware coding
// workflows. The implementation lives in internal packages so providers and
// persistence can be exercised with fakes in tests.
package main

import (
	"context"
	"os"

	"github.com/basant/rolemux/internal/cli"
)

var version = "dev"

func main() {
	code := cli.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version)
	os.Exit(code)
}
