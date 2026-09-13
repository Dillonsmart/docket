// Command docket records, per commit, how agent-written code came to exist and
// what verified it.
package main

import (
	"os"

	"github.com/Dillonsmart/docket/internal/cli"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)" ./cmd/docket
var version = "0.1.0-dev"

func main() {
	cli.Version = version
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
