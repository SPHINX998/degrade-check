//go:build !windows && !darwin

package relaycheckclient

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const systemdUnit = "pinai-check"

type osScheduler struct{}

// NewScheduler is a systemd user timer on Linux.
func NewScheduler() Scheduler { return osScheduler{} }

func systemdDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "systemd", "user"), nil
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("系统定时任务操作失败：%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (osScheduler) Install(exe string, hours int) error {
	dir, err := systemdDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	service := fmt.Sprintf("[Unit]\nDescription=PinAI Check\n\n[Service]\nType=oneshot\nExecStart=%q run --scheduled\n", exe)
	timer := fmt.Sprintf("[Unit]\nDescription=PinAI Check timer\n\n[Timer]\nOnBootSec=10min\nOnUnitActiveSec=%dh\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n", hours)
	if err := os.WriteFile(filepath.Join(dir, systemdUnit+".service"), []byte(service), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, systemdUnit+".timer"), []byte(timer), 0o644); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	return systemctl("enable", "--now", systemdUnit+".timer")
}

func (osScheduler) Remove() error {
	dir, err := systemdDir()
	if err != nil {
		return err
	}
	_ = systemctl("disable", "--now", systemdUnit+".timer")
	for _, name := range []string{systemdUnit + ".timer", systemdUnit + ".service"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
