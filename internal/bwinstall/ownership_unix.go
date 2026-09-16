//go:build linux || darwin

package bwinstall

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// ownedByPrivateGroup reports whether dir belongs to the account running this
// process and to that account's own private group, the per-user group Debian
// and Fedora hosts create alongside the account. Such a group has exactly one
// member, so its write bit hands the directory to nobody the owner is not
// already.
//
// Anything it cannot prove counts as not private: an unreadable passwd or group
// database, or a filesystem whose stat result carries no uid and gid, leaves
// prepareDir refusing a directory it cannot vouch for.
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
