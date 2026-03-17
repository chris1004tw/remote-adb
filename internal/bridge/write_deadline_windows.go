//go:build windows

package bridge

// Windows + Go 1.26 在高頻 deadline/timer 壓力下曾出現 runtime netpoll 崩潰。
// 對高吞吐 DataChannel（scrcpy/camera）而言，每次 WRTE 都設/清 write deadline
// 會放大這條路徑，因此 Windows 先停用這個保護，優先避免整個 GUI process 直接 crash。
func writeDeadlinesEnabled() bool {
	return false
}
