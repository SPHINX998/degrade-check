//go:build darwin

package relaycheckclient

import (
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const keychainService = "PinAICheck"

// keychainSealer keeps the key in the login keychain. The sealed file only
// records that a key exists; the bytes stay in the keychain.
type keychainSealer struct{}

func newPlatformSealer(string) keySealer { return keychainSealer{} }

func (keychainSealer) Level() string { return "macOS 钥匙串" }

func keychain(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/security", args...).Output()
	if err != nil {
		return "", fmt.Errorf("钥匙串操作失败：%w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (keychainSealer) Seal(key []byte) ([]byte, error) {
	if _, err := keychain("add-generic-password", "-U", "-s", keychainService, "-a", "store-key", "-w", hex.EncodeToString(key)); err != nil {
		return nil, err
	}
	return []byte("keychain"), nil
}

func (keychainSealer) Unseal([]byte) ([]byte, error) {
	value, err := keychain("find-generic-password", "-s", keychainService, "-a", "store-key", "-w")
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(value)
}

func hideWindow(*exec.Cmd) {}
