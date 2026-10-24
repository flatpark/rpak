// Command rpak prefetches flatpak extra-data concurrently, then runs flatpak.
package main

import (
	"os"

	"github.com/flatpark/rpak/internal/app"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(app.Main(version))
}
