package api

import "golang.org/x/sys/unix"

// diskFreeBytes returns the free space (bytes) on the filesystem holding path,
// or 0 if it cannot be determined.
func diskFreeBytes(path string) uint64 {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0
	}
	return st.Bavail * uint64(st.Bsize)
}
