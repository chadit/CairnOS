package cli

import "errors"

// Sentinel errors for the CLI.
var (
	// errValueHasNewline reports a vault value the generated environment file
	// cannot carry: both readers parse line by line, so the remainder would be
	// read as a second assignment.
	errValueHasNewline = errors.New("a vault value contains a newline, which " +
		"neither systemd nor a sourcing shell can read back; fix the field on the item")

	// errNoVaultItem reports that neither the config block nor the credentials
	// file names the vault item to read.
	errNoVaultItem = errors.New("no vault item is configured; " +
		"set [secrets.bitwarden] item, or run `cairnos secrets auth` to write")

	// errNoFieldsOnItem reports an item carrying nothing the pull can use: no
	// field named like a variable, and no mapping table to interpret the rest.
	errNoFieldsOnItem = errors.New("no field on the vault item is named like a " +
		"CAIRNOS_ variable, and [secrets.bitwarden.fields] maps none; item")

	// errNotATerminal reports a stdin that is a pipe or a file, where there is
	// no echo to turn off and nothing on screen to hide.
	errNotATerminal = errors.New("stdin is not a terminal")

	// errNoPinnedBitwarden reports a platform this build has no pinned release
	// for, which is every platform but the two CairnOS runs on.
	errNoPinnedBitwarden = errors.New("no Bitwarden CLI release is pinned for")

	// errNoValueForPrompt reports a prompt that had nothing to answer it: no
	// environment value, no default, and a stdin that is done. Failing here beats
	// blocking a deploy on a terminal that will never arrive.
	errNoValueForPrompt = errors.New("no value was given and there is no default; " +
		"set it in the environment or run this on a terminal")

	// errNoBitwardenBinary reports a missing bw CLI on a platform this build
	// pins no release for, where CairnOS has nothing of its own to fall back to.
	errNoBitwardenBinary = errors.New("the bw CLI is not on PATH; " +
		"install it from your package manager or from the Bitwarden releases page")

	// ErrBadInput reports a malformed or uncoercible --input flag value.
	ErrBadInput = errors.New("bad --input value")

	// ErrScheduledRunFailed reports a scheduled fire whose run finished in a
	// non-succeeded status; the daemon logs it and keeps running.
	ErrScheduledRunFailed = errors.New("scheduled run failed")

	// errOAuthNeedsAuth reports an OAuth login configured with auth disabled: a
	// minted token is only useful if the interceptor verifies it.
	errOAuthNeedsAuth = errors.New("oauth requires auth")

	// errNoSourceCheckout reports that `cairnos update` found nothing to install
	// and nothing to build: no release carries an asset for this platform, the
	// directory around the running binary carries no go.mod and .git, and no
	// --source named one that does.
	errNoSourceCheckout = errors.New("no CairnOS release for this platform and no " +
		"checkout to update; publish a release or pass --source <dir>")

	// errNoBuildInfo reports a binary with no embedded build information, which
	// leaves the update no module path to derive the release repository from.
	errNoBuildInfo = errors.New("this binary carries no build information")

	// errNotAGitHubModule reports a module path the release lookup cannot read
	// an owner and repository out of, such as a fork hosted elsewhere.
	errNotAGitHubModule = errors.New("the module path is not a github.com repository")

	// errReleaseAPI reports a release API call or an asset download that
	// answered with something other than a result or a plain "no release".
	errReleaseAPI = errors.New("the GitHub release API answered unexpectedly")

	// errNoTagRef reports a release whose tag the API does not resolve, which
	// leaves the update no commit to compare the running build against.
	errNoTagRef = errors.New("the release tag resolves to no commit")

	// errNoChecksumsAsset reports a release that publishes this platform's
	// binary with no checksums beside it. That is a broken release rather than
	// a missing one, so the update stops instead of quietly building the clone.
	errNoChecksumsAsset = errors.New("the release publishes no checksums to verify its assets against")

	// errNoChecksumLine reports a checksums.txt that lists no digest for this
	// platform's asset, so the download has nothing to be checked against.
	errNoChecksumLine = errors.New("the release checksums list no digest for")

	// errAssetDigest reports a downloaded asset whose sha256 is not the one the
	// release publishes. Nothing is installed.
	errAssetDigest = errors.New("the downloaded release asset does not match its published sha256")

	// errAssetStamp reports a release asset whose own vcs stamp is absent,
	// dirty, or a commit other than the one its tag names, which means the
	// artifact and the tag are not the same code.
	errAssetStamp = errors.New("the release asset was not built from the tagged commit")

	// errNoGoToolchain reports a host with no go on PATH and none at the
	// tarball's install prefix, where the update has nothing to build with.
	errNoGoToolchain = errors.New("no go toolchain on PATH or at " + goFallbackPath)

	// errNoHomeDir reports an environment that names no home directory, which
	// leaves the paths resolved under it with nothing to resolve against.
	errNoHomeDir = errors.New("the environment names no home directory")

	// errGitCountShape reports output from `git rev-list --left-right --count`
	// that is not the two numbers the fast-forward decision reads.
	errGitCountShape = errors.New("unexpected git commit-count output")

	// errLinkNotASymlink reports a regular file where the ~/.local/bin entry
	// belongs. It may be an install the update never placed, so it stays.
	errLinkNotASymlink = errors.New("refusing to replace a regular file with a symlink")

	// errServiceSlowStart reports a unit that never came back on a new main
	// process within the restart window.
	errServiceSlowStart = errors.New("the service did not come back in time")

	// errServiceStale reports a service that restarted onto a build other than
	// the one the update produced.
	errServiceStale = errors.New("the service came back on the wrong build")

	// errUpdateBusy reports an update lock another update holds, which is how
	// `cairnos update` and serve's loop keep from building in the same checkout
	// or replacing the same binary at once.
	errUpdateBusy = errors.New("another update is already running on this host")

	// errDirtyCheckout reports a working tree carrying changes, which serve's
	// update loop refuses to build: the result would be stamped "+dirty" and read
	// as stale again after the restart.
	errDirtyCheckout = errors.New("the checkout carries uncommitted changes")

	// errDivergedCheckout reports a checkout holding commits its upstream lacks,
	// which serve's update loop refuses to build because the result would not be
	// the upstream revision it wanted.
	errDivergedCheckout = errors.New("the checkout has commits its upstream lacks")

	// errBuildStamp reports a new binary whose stamp is not the revision the
	// update wanted. Serve's update loop stops there rather than restart onto a
	// build it would judge stale and rebuild on every start after.
	errBuildStamp = errors.New("the new build is not stamped with the wanted revision")

	// errFlagValue reports a flag whose value cannot work. Catching it here beats
	// finding out from a server that started on the wrong port: --port 0 makes
	// llama-server bind an ephemeral port while every probe still targets :0, so
	// an upgrade would roll back a runtime that was running fine.
	errFlagValue = errors.New("model: flag value out of range")
)
