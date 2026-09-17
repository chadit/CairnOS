package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/chadit/CairnOS/internal/bwinstall"
)

// bwReleaseT is the release tag the version in bwpin_gen.go belongs to, and
// bwReleaseBase where its assets live. The version and the four digests are
// generated together by `go run ./tools/bwpin`, because a digest belonging to
// another release fails the install rather than being accepted quietly.
const (
	bwReleaseT    = "cli-v" + bwVersion
	bwReleaseBase = "https://github.com/bitwarden/clients/releases/download"
)

// cmdSecretsInstallBW installs the pinned Bitwarden CLI. The alternative is a
// README block that fetches a zip, checks a hash, and unpacks it, which is
// several steps an operator can get almost right and end up with an unverified
// binary. The flag is --bin-dir, not --dir, because --dir names the CairnOS home
// everywhere else under `secrets`, which this command never uses.
func (a *App) cmdSecretsInstallBW(ctx context.Context, args []string) int {
	const label = "secrets install-bw"

	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	binDir := flags.String("bin-dir", "", "install into this directory instead of ~/.local/bin")
	force := flags.Bool("force", false, "reinstall even when the pinned version is already there")

	if code, proceed := parseFlags(flags, args); !proceed {
		return code
	}

	dir, err := bwInstallDir(*binDir)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	result, err := a.installBitwarden(ctx, dir, *force)
	if err != nil {
		a.printErrf("%s: %v\n", label, err)

		return ExitFailure
	}

	if !result.Installed {
		a.printOutf("%s: %s is already version %s\n", label, result.Path, result.Version)

		return ExitOK
	}

	a.printOutf("%s: installed %s (version %s)\n", label, result.Path, result.Version)

	return ExitOK
}

// installBitwarden runs one install against the pinned release for this
// platform.
func (a *App) installBitwarden(ctx context.Context, dir string, force bool) (bwinstall.Result, error) {
	asset, err := a.bwPin(runtime.GOOS)
	if err != nil {
		return bwinstall.Result{}, err
	}

	result, err := bwinstall.Install(ctx, &bwinstall.Options{
		URL:           asset.url,
		ArchiveSHA256: asset.archiveSum,
		BinarySHA256:  asset.binarySum,
		Version:       bwVersion,
		Dest:          filepath.Join(dir, bwBinary),
		Logger:        a.logger,
		Force:         force,
	})
	if err != nil {
		return bwinstall.Result{}, fmt.Errorf("install the Bitwarden CLI: %w", err)
	}

	return result, nil
}

// bwAsset is one platform's pinned download: where it comes from and the two
// digests it has to match.
type bwAssetPin struct {
	url        string
	archiveSum string
	binarySum  string
}

// bwAsset names the release asset for one platform. Only the two CairnOS runs
// on are pinned, and an unpinned platform is refused rather than guessed at,
// because there would be no digest to check the download against.
func bwAsset(goos string) (bwAssetPin, error) {
	pins := map[string]struct {
		platform   string
		archiveSum string
		binarySum  string
	}{
		"linux":  {platform: "linux", archiveSum: bwLinuxArchiveSHA256, binarySum: bwLinuxBinarySHA256},
		"darwin": {platform: "macos", archiveSum: bwMacOSArchiveSHA256, binarySum: bwMacOSBinarySHA256},
	}

	pin, pinned := pins[goos]
	if !pinned {
		return bwAssetPin{}, fmt.Errorf("%w: %s", errNoPinnedBitwarden, goos)
	}

	return bwAssetPin{
		url:        fmt.Sprintf("%s/%s/bw-%s-%s.zip", bwReleaseBase, bwReleaseT, pin.platform, bwVersion),
		archiveSum: pin.archiveSum,
		binarySum:  pin.binarySum,
	}, nil
}

// WithBitwardenPin replaces the release pin for this App and returns it for
// chaining. Only a test build calls it: every vault command hashes the bw it
// resolves, and a suite's fake bw is a symlink to its own test binary, which no
// published release will ever hash to. source is the asset an install would
// fetch, so a case meaning to reach the installer can point it at something
// that fails loudly rather than downloading the real CLI.
func (a *App) WithBitwardenPin(source, binarySum string) *App {
	pin := bwAssetPin{url: source, binarySum: binarySum}
	a.bwPin = func(string) (bwAssetPin, error) { return pin, nil }

	return a
}

// WithoutBitwardenPin makes every platform look unpinned, and returns the App
// for chaining. Only a test build calls it: runtime.GOOS is fixed at compile
// time, so this is the only way to reach the path a platform outside the pin
// table takes.
func (a *App) WithoutBitwardenPin() *App {
	a.bwPin = func(goos string) (bwAssetPin, error) {
		return bwAssetPin{}, fmt.Errorf("%w: %s", errNoPinnedBitwarden, goos)
	}

	return a
}

// bwInstallDir resolves where the binary goes, defaulting to the personal bin
// directory a login shell already has on its PATH.
func bwInstallDir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(home, ".local", "bin"), nil
}
