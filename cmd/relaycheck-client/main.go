// Command relaycheck-client is the portable checker. Started plainly it opens a
// local page; `run --scheduled` is the background run the OS scheduler starts.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/SPHINX998/degrade-check/internal/features/relaycheckclient"
)

// version is stamped at build time.
var version = "0.3.2"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func machine() (relaycheckclient.Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return relaycheckclient.Env{}, err
	}
	return relaycheckclient.Env{Home: home, Getenv: os.Getenv, LookPath: exec.LookPath}, nil
}

func run(args []string) error {
	env, err := machine()
	if err != nil {
		return err
	}
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	if command == "scan" {
		return json.NewEncoder(os.Stdout).Encode(relaycheckclient.Scan(env))
	}
	dir, err := relaycheckclient.DefaultStoreDir()
	if err != nil {
		return err
	}
	store, err := relaycheckclient.OpenStore(dir)
	if err != nil {
		return err
	}
	server := relaycheckclient.DefaultServer
	if override := strings.TrimSpace(os.Getenv("PINAI_CHECK_SERVER")); override != "" {
		server = override
	}
	// No client timeout: a test is a long stream, bounded by its own context.
	netLog := relaycheckclient.NewNetLog(server)
	runner := &relaycheckclient.Runner{Server: server, Client: &http.Client{Transport: netLog.Transport(http.DefaultTransport)},
		Exec: func(ctx context.Context, name string, args []string, stdin string, env []string) ([]byte, []byte, error) {
			netLog.RecordCommand(name)
			return relaycheckclient.RunCommand(ctx, name, args, stdin, env)
		}}
	ctx := context.Background()

	if command == "run" {
		note, err := relaycheckclient.RunScheduled(ctx, store, runner, env)
		if note != "" {
			fmt.Println(note)
		}
		return err
	}

	if alreadyRunning() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	app := &relaycheckclient.App{Env: env, Store: store, Runner: runner, Scheduler: relaycheckclient.NewScheduler(), Exe: exe, Version: version, OpenURL: openBrowser, NetLog: netLog}
	listener, url, err := app.Listen()
	if err != nil {
		return err
	}
	app.ReinstallSchedule()
	fmt.Println(url)
	return runDesktop(ctx, app, listener, url, dir)
}

// openInBrowser opens the page shortly after the server starts listening.
func openInBrowser(url string) {
	if os.Getenv("PINAI_CHECK_NO_BROWSER") != "" {
		return
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		openBrowser(url)
	}()
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
