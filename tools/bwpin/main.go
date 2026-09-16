// Command bwpin refreshes the pinned Bitwarden CLI release recorded in
// internal/cli/bwpin_gen.go. The work lives in internal/bwpin; this parses
// flags and turns a failure into an exit code.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/chadit/CairnOS/internal/bwpin"
)

func main() {
	var cfg bwpin.Config

	flag.StringVar(&cfg.Out, "out", bwpin.DefaultOut, "the generated pin file to rewrite")
	flag.StringVar(&cfg.API, "api", bwpin.DefaultAPI, "base URL of the GitHub API")
	flag.StringVar(&cfg.Download, "download", bwpin.DefaultDownload, "base URL of the release assets")
	flag.Parse()

	cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := bwpin.Run(context.Background(), cfg); err != nil {
		cfg.Logger.Error("refresh the Bitwarden CLI pin", "error", err)

		os.Exit(1)
	}
}
