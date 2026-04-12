//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos

package main

import (
	"golang.org/x/sys/unix"
)

// enableKeypress puts the terminal into a mode where individual keypresses
// can be read without waiting for Enter.  Unlike term.MakeRaw it preserves
// output post-processing (OPOST) so \n still produces \r\n, and keeps ISIG
// so Ctrl-C still generates SIGINT.
//
// Returns a function that restores the original terminal state.
func enableKeypress(fd int) (restore func(), err error) {
	old, err := unix.IoctlGetTermios(fd, ioctlGet)
	if err != nil {
		return nil, err
	}

	modified := *old
	modified.Lflag &^= unix.ECHO | unix.ICANON
	modified.Cc[unix.VMIN] = 1
	modified.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, ioctlSet, &modified); err != nil {
		return nil, err
	}

	return func() {
		unix.IoctlSetTermios(fd, ioctlSet, old)
	}, nil
}
