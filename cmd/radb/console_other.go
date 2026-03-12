//go:build !windows

package main

// freeConsole 非 Windows 平台不需處理主控台。
// Linux / macOS 上 CLI 程式天然繼承父行程的 stdout/stderr，無需額外處理。
// 此空實作配合 console_windows.go 的 build tag 機制，確保跨平台編譯正常。
func freeConsole() {}
