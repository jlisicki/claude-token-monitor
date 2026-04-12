//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

const ioctlGet = unix.TIOCGETA
const ioctlSet = unix.TIOCSETA
