//go:build !linux

package worker

import "os"

func privateInputOwnedByCurrentUser(os.FileInfo) bool {
	return false
}
