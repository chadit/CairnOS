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

// The pinned release. Upgrading means changing the version and all four digests
// together, since a digest belonging to another release fails the install
// rather than being accepted quietly.
//
// Each pair was taken by downloading the asset from
// https://github.com/bitwarden/clients/releases/download/cli-v2026.8.0/ and
// running sha256sum on the zip and on the single `bw` entry unpacked from it.
// The binary digest is the one a later run compares the installed file against,
// so it has to come from the same download as the archive digest.
const (
	bwVersion  = "2026.8.0"
	bwReleaseT = "cli-v" + bwVersion

	bwLinuxArchiveSHA256 = "367f618e9fcccaac4980ec12c7bafd01df739b5f3cb1af31bc9045cf75eea1d6"
	bwLinuxBinarySHA256  = "73b6e9b5706b6ba315974ada6723b5bb3e54b51b99583a58d9968925b607cd9c"

	bwMacOSArchiveSHA256 = "c5d57f70d5394f8c348f6c3bf53683ad6d15e6acfe55e7c1e0a8f376482d8e71"
	bwMacOSBinarySHA256  = "16219f8c8e5fd1bc8211ea1294cb866fc6a100d7bb43b79814afeabfbe1c11eb"
)

// bwReleaseBase is where the assets live.
const bwReleaseBase = "https://github.com/bitwarden/clients/releases/download"

// cmdSecretsInstallBW installs the pinned Bitwarden CLI. The alternative is a
// README block that fetches a zip, checks a hash, and unpacks it, which is
// several steps an operator can get almost right and end up with an unverified
// binary.
func (a *App) cmdSecretsInstallBW(ctx context.Context, args []string) int {
	const label = "secrets install-bw"

	flags := flag.NewFlagSet(label, flag.ContinueOnError)
	flags.SetOutput(a.stderr)

	// Not --dir: that flag names the CairnOS home everywhere else under
	// `secrets`, and this command has no use for the home at all.
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
	asset, err := bwAsset(runtime.GOOS)
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

// bwInstallDir resolves where the binary goes.
func bwInstallDir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	// The personal bin directory, which a login shell already has on its PATH.
	return filepath.Join(home, ".local", "bin"), nil
}
