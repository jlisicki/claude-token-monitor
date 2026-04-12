//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !zos

package main

import "fmt"

func enableKeypress(fd int) (restore func(), err error) {
	return nil, fmt.Errorf("keypress detection not supported on this platform")
}
