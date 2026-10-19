//go:build windows

package bwinstall

import "os"

func ownedByPrivateGroup(os.FileInfo) bool {
	// dirTakeoverRisk returns before asking on Windows, where the POSIX bits it
	// would excuse carry no signal. The build still needs the symbol.
	return false
}
