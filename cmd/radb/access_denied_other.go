//go:build !windows

package main

func isWindowsAccessDenied(error) bool {
	return false
}
