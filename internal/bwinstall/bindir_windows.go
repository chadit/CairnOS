//go:build windows

package bwinstall

import "os"

// dirTakeoverRisk accepts every install directory on Windows. Mode().Perm()
// reports 0777 for a directory there no matter what its ACLs say, so the POSIX
// bits carry no signal at all, and the directory this tool creates sits under
// the caller's own profile with the ACLs Windows gives it.
func dirTakeoverRisk(string, os.FileInfo) error {
	return nil
}
