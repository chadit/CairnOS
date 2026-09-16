package bwpin

import "errors"

// Failures this package reports on its own, rather than passing one up from
// the standard library. They are exported because a caller that wants to tell
// a moved tag from a bad network has nothing else to match on.
var (
	// ErrNoCLIRelease reports a feed with no release matching the CLI tag
	// pattern, which means the pattern moved and hashing anything found now
	// would pin the wrong artifact.
	ErrNoCLIRelease = errors.New("no cli-v release was found in the first pages of")

	// ErrNotHTTPS reports a plain http source. Nothing authenticates where
	// those bytes came from, and these bytes become a compiled-in digest.
	ErrNotHTTPS = errors.New("the URL must be https")

	// ErrBadStatus reports a response that is not 200, so a release host
	// serving an error page fails here instead of at the digest comparison.
	ErrBadStatus = errors.New("unexpected HTTP status")

	// ErrTooLarge reports a response past the size cap.
	ErrTooLarge = errors.New("the response is past the size limit")

	// ErrArchiveLayout reports an archive that is not the one-file zip the
	// installer unpacks, so a layout change stops the bump rather than
	// producing digests that describe a file the install never writes.
	ErrArchiveLayout = errors.New("the archive is not a single bw entry")

	// ErrNoVersionLine reports a generated file with no bwVersion constant to
	// read the current pin from.
	ErrNoVersionLine = errors.New("no bwVersion line was found in")
)
