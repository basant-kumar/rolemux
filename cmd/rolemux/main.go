// Command rolemux is the thin host-facing CLI for durable, role-aware coding
// workflows. The implementation lives in internal packages so providers and
// persistence can be exercised with fakes in tests.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/basant-kumar/rolemux/internal/cli"
)

const developmentVersion = "dev"

var version = developmentVersion

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, buildVersion(version))
	os.Exit(code)
}

func buildVersion(injected string) string {
	if candidate := strings.TrimSpace(injected); candidate != "" && candidate != developmentVersion {
		return candidate
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if candidate := strings.TrimSpace(info.Main.Version); candidate != "" && candidate != "(devel)" {
			return candidate
		}
	}
	return developmentVersion
}
