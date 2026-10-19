//go:build !unix

package cli

// fileOwner reports no owner off unix: Windows files carry an ACL rather than
// a uid, and the rename keeps no ownership a chown could restore.
func fileOwner(string) (int, int, bool, error) {
	return 0, 0, false, nil
}
