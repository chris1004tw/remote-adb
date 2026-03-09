//go:build !windows

package main

// attachParentConsole 非 Windows 平台不需處理主控台附加。
func attachParentConsole() {}
