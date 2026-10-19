package cli

import "fmt"

// KeepOwnerPlan decides how a replace by euid keeps a file owned by ownerUID.
// The rename installs a file the caller created, so a sudo edit of serve's
// 0600 config would leave one serve cannot read. Root chowns it back; anyone
// else refuses. Exported because tests cannot create another user's file.
func KeepOwnerPlan(path string, ownerUID, euid int) (bool, error) {
	switch {
	case ownerUID == euid:
		return false, nil
	case euid == 0:
		return true, nil
	default:
		return false, fmt.Errorf("%w: %s is owned by uid %d and this command runs as uid %d; run it as that user, or as root so the owner is kept",
			errForeignOwner, path, ownerUID, euid)
	}
}
