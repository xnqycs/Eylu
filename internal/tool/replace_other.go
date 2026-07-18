//go:build !windows

package tool

import "os"

func replaceAtomically(source, target string) error {
	return os.Rename(source, target)
}
