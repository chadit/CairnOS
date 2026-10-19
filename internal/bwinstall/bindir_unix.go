//go:build linux || darwin

package bwinstall

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

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
