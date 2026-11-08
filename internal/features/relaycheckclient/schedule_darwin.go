//go:build darwin

package relaycheckclient

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const launchLabel = "com.pinaic.check"

type osScheduler struct{}

// NewScheduler is launchd on macOS.
func NewScheduler() Scheduler { return osScheduler{} }

func launchPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist"), nil
}

func launchDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func xmlEscape(value string) string {
	var out strings.Builder
	_ = xml.EscapeText(&out, []byte(value))
	return out.String()
}

func (osScheduler) Install(exe string, hours int) error {
	path, err := launchPlistPath()
	if err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>run</string><string>--scheduled</string></array>
<key>StartInterval</key><integer>%d</integer>
</dict></plist>
`, launchLabel, xmlEscape(exe), hours*3600)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", launchDomain()+"/"+launchLabel).Run()
	if out, err := exec.Command("launchctl", "bootstrap", launchDomain(), path).CombinedOutput(); err != nil {
		return fmt.Errorf("系统定时任务操作失败：%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (osScheduler) Remove() error {
	path, err := launchPlistPath()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", launchDomain()+"/"+launchLabel).Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
