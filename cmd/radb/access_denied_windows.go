//go:build windows

package main

import (
	"errors"
	"syscall"
)

func isWindowsAccessDenied(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
