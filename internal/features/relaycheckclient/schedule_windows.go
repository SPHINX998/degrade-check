//go:build windows

package relaycheckclient

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type osScheduler struct{}

// NewScheduler is Task Scheduler on Windows.
func NewScheduler() Scheduler { return osScheduler{} }

func scheduleArgs(exe string, hours int) []string {
	unit, every := "HOURLY", hours
	if hours >= 24 {
		unit, every = "DAILY", max(1, hours/24)
	}
	return []string{"/Create", "/F", "/TN", ScheduleTask, "/SC", unit, "/MO", strconv.Itoa(every), "/TR", `"` + exe + `" run --scheduled`}
}

func schtasks(args ...string) error {
	cmd := exec.Command("schtasks", args...)
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("系统定时任务操作失败：%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (osScheduler) Install(exe string, hours int) error { return schtasks(scheduleArgs(exe, hours)...) }

func (osScheduler) Remove() error {
	cmd := exec.Command("schtasks", "/Query", "/TN", ScheduleTask)
	hideWindow(cmd)
	if cmd.Run() != nil {
		return nil // nothing to remove
	}
	return schtasks("/Delete", "/F", "/TN", ScheduleTask)
}
