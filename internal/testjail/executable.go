package testjail

import (
	"os"
	"syscall"
)

func WriteExecutable(path string, body []byte, mode os.FileMode) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(path, body, mode)
}

// TrustExecutable takes group and world write off path. The daemon starts only
// a binary neither may write, and `go test` links the test binary under the
// caller's umask: under 002 every test that starts its own binary as the
// daemon would be refused.
func TrustExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 == 0 {
		return nil
	}
	return os.Chmod(path, info.Mode().Perm()&^0o022)
}
