//go:build linux

package mirrorhost

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal pair: the master muster-mirror reads and
// writes, and the slave the tmux client runs on.
//
// ⚠ THE ioctls GO THROUGH SyscallConn, NEVER (*os.File).Fd. Fd switches the
// descriptor to blocking mode, and a blocking read on the master is then not
// interrupted by Close — a stream muster ended would leave its reader goroutine
// parked on a pty nobody writes to. Through SyscallConn the master stays in the
// runtime poller, so Close unblocks the read.
func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	var n int
	var ierr error
	if err := control(m, func(fd int) {
		if ierr = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); ierr != nil {
			return
		}
		n, ierr = unix.IoctlGetInt(fd, unix.TIOCGPTN)
	}); err != nil || ierr != nil {
		_ = m.Close()
		return nil, nil, fmt.Errorf("unlock pty: %v %v", err, ierr)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = m.Close()
		return nil, nil, fmt.Errorf("open pty slave: %w", err)
	}
	return m, s, nil
}

// setSize applies a terminal size to the pty.
func setSize(f *os.File, cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	var ierr error
	if err := control(f, func(fd int) {
		ierr = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
	}); err != nil {
		return err
	}
	return ierr
}

func control(f *os.File, fn func(fd int)) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	return rc.Control(func(fd uintptr) { fn(int(fd)) })
}

// ttyAttr makes the child a session leader with the slave (its fd 0) as its
// controlling terminal, which is what a tmux client needs to run.
func ttyAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
}
