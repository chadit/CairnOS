package bwinstall

import (
	"os"
	"runtime"
)

// writableByOthers are the mode bits that would let another account replace the
// installed binary between one run and the next, and writableByWorld the half of
// them no ownership check can ever excuse.
const (
	writableByOthers = 0o022
	writableByWorld  = 0o002
)

// dirTakeoverRisk refuses an install directory another account can write, since
// swapping the binary between runs is what verifying it at install time cannot
// catch. A directory in the owner's own private group is the exception: Debian
// and Fedora ship ~/.local/bin that way, so the group bit adds no second writer.
// World-writable stays refused.
func dirTakeoverRisk(dir string, info os.FileInfo) error {
	// Mode().Perm() reports 0777 for a Windows directory whatever its ACLs say,
	// so the bits carry no signal there, and the directory this tool creates sits
	// under the caller's own profile with the ACLs Windows gives it.
	if runtime.GOOS == "windows" {
		return nil
	}

	mode := info.Mode().Perm()
	if mode&writableByOthers == 0 {
		return nil
	}

	if mode&writableByWorld == 0 && ownedByPrivateGroup(info) {
		return nil
	}

	return installError(CodeBinDir,
		"%s is mode %04o, which lets another account replace the binary; "+
			"tighten it to 0755 or install elsewhere with --bin-dir", dir, mode)
}
