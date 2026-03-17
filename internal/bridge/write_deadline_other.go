//go:build !windows

package bridge

func writeDeadlinesEnabled() bool {
	return true
}
