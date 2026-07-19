//go:build !windows

package skilldist

import "os"

func replaceFile(source, target string) error { return os.Rename(source, target) }
