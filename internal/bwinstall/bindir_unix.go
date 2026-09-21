//go:build linux || darwin

package bwinstall

import (
	"os"
	"os/user"
	"strconv"
	"syscall"

	"github.com/chadit/CairnOS/internal/cairnerr"
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
	mode := info.Mode().Perm()
	if mode&writableByOthers == 0 {
		return nil
	}

	if mode&writableByWorld == 0 && ownedByPrivateGroup(info) {
		return nil
	}

	return cairnerr.Newf(CodeBinDir,
		"%s is mode %04o, which lets another account replace the binary; "+
			"tighten it to 0755 or install elsewhere with --bin-dir", dir, mode)
}

// ownedByPrivateGroup reports whether the directory belongs to this process's
// account and to that account's own per-user group. Such a group has one
// member, so its write bit hands the directory to nobody the owner is not
// already. Anything it cannot prove counts as not private, leaving
// dirTakeoverRisk to refuse.
func ownedByPrivateGroup(info os.FileInfo) bool {
	stat, isUnix := info.Sys().(*syscall.Stat_t)
	if !isUnix {
		return false
	}

	if int(stat.Uid) != os.Getuid() {
		return false
	}

	owner, err := user.LookupId(strconv.FormatUint(uint64(stat.Uid), 10))
	if err != nil {
		return false
	}

	group, err := user.LookupGroupId(strconv.FormatUint(uint64(stat.Gid), 10))
	if err != nil {
		return false
	}

	return group.Name == owner.Username
}
