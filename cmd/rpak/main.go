// Command rpak prefetches flatpak extra-data concurrently, then runs flatpak.
package main

import (
	"os"

	"github.com/flatpark/rpak/internal/app"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// selfUpdate is "disabled" (-ldflags "-X main.selfUpdate=disabled") in a
// build for a package manager: rpak self-update then points to it.
var selfUpdate = "enabled"

func main() {
	os.Exit(app.Main(version, selfUpdate))
}
