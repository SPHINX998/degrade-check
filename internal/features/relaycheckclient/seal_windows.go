//go:build windows

package relaycheckclient

import (
	"errors"
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32Local          = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree          = kernel32Local.NewProc("LocalFree")
)

const cryptProtectUIForbidden = 0x1

type dataBlob struct {
	size uint32
	data *byte
}

func newBlob(data []byte) *dataBlob {
	if len(data) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{size: uint32(len(data)), data: &data[0]}
}

func (b *dataBlob) bytes() []byte {
	out := make([]byte, b.size)
	copy(out, unsafe.Slice(b.data, b.size))
	return out
}

// dpapiSealer protects the key with the Windows user's own credentials: another
// account, or the file on another machine, cannot open it.
type dpapiSealer struct{}

func newPlatformSealer(string) keySealer { return dpapiSealer{} }

func (dpapiSealer) Level() string { return "Windows 系统加密（只有当前用户能解开）" }

func (dpapiSealer) Seal(key []byte) ([]byte, error) {
	var out dataBlob
	ok, _, err := procCryptProtectData.Call(uintptr(unsafe.Pointer(newBlob(key))), 0, 0, 0, 0,
		cryptProtectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if ok == 0 {
		return nil, errors.Join(errors.New("CryptProtectData"), err)
	}
	defer func() { _, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(out.data))) }()
	return out.bytes(), nil
}

func (dpapiSealer) Unseal(sealed []byte) ([]byte, error) {
	var out dataBlob
	ok, _, err := procCryptUnprotectData.Call(uintptr(unsafe.Pointer(newBlob(sealed))), 0, 0, 0, 0,
		cryptProtectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if ok == 0 {
		return nil, errors.Join(errors.New("CryptUnprotectData"), err)
	}
	defer func() { _, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(out.data))) }()
	return out.bytes(), nil
}

// hideWindow keeps a CLI from flashing a console when started from the
// background schedule.
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
