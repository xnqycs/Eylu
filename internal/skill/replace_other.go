//go:build !windows

package skill

import "os"

func replaceTrustFile(source, target string) error {
	return os.Rename(source, target)
}
