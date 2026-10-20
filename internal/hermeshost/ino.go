package hermeshost

import (
	"os"
	"syscall"
)

// fileIno is path's inode — of the path itself, never a symlink's target —
// 0 when it does not exist (or cannot be stat'ed), 1 on a platform with no
// inode in its stat.
func fileIno(path string) uint64 {
	st, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Ino != 0 {
		return uint64(sys.Ino)
	}
	return 1
}
