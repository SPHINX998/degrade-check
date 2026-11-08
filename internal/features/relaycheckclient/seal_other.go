//go:build !windows && !darwin

package relaycheckclient

import "os/exec"

// fileSealer is the weakest level: Linux has no keystore every machine carries,
// so the key is only as safe as the owner-only file permission, and the page
// says so. The store directory is created 0700.
type fileSealer struct{}

func newPlatformSealer(string) keySealer { return fileSealer{} }

func (fileSealer) Level() string {
	return "文件权限保护（Linux 没有统一的系统密钥库）"
}

func (fileSealer) Seal(key []byte) ([]byte, error) { return append([]byte(nil), key...), nil }

func (fileSealer) Unseal(sealed []byte) ([]byte, error) { return sealed, nil }

func hideWindow(*exec.Cmd) {}
