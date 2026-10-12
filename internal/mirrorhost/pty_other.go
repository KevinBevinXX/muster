//go:build !linux

package mirrorhost

import (
	"errors"
	"os"
	"syscall"
)

// errNoPTY: the pty is implemented for Linux only (the hosts this was built for
// are NixOS machines). Session LISTING works everywhere; attach answers this.
var errNoPTY = errors.New("muster-mirror: terminal attach is implemented for Linux only")

func openPTY() (master, slave *os.File, err error) { return nil, nil, errNoPTY }

func setSize(*os.File, uint16, uint16) error { return errNoPTY }

func ttyAttr() *syscall.SysProcAttr { return nil }
