//go:build aix || linux || solaris || zos

package main

import "golang.org/x/sys/unix"

const ioctlGet = unix.TCGETS
const ioctlSet = unix.TCSETS
