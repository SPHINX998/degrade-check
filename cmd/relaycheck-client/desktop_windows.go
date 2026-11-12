//go:build windows

package main

import (
	"context"
	_ "embed"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"fyne.io/systray"
	"github.com/SPHINX998/degrade-check/internal/features/relaycheckclient"
	webview2 "github.com/jchv/go-webview2"
)

const windowTitle = "降智检测工具"

// iconResourceID is the icon group rsrc gives the icon when the manifest takes id 1.
const iconResourceID = 2

//go:embed icon.ico
var trayIcon []byte

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procSetWindowLongPtr = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProc   = user32.NewProc("CallWindowProcW")
	procShowWindow       = user32.NewProc("ShowWindow")
	procSetForeground    = user32.NewProc("SetForegroundWindow")
	procFindWindow       = user32.NewProc("FindWindowW")
	procCreateMutex      = kernel32.NewProc("CreateMutexW")
	previousWindowProc   uintptr
	singleInstanceHandle uintptr
)

const (
	wmClose            = 0x0010
	wmSysCommand       = 0x0112
	scMinimize         = 0xF020
	swHide             = 0
	swRestore          = 9
	gwlpWndProc        = ^uintptr(3) // -4
	errorAlreadyExists = 183
)

// The WebView2 window must stay on the thread that made it.
func init() { runtime.LockOSThread() }

func showWindow(handle uintptr) {
	_, _, _ = procShowWindow.Call(handle, swRestore)
	_, _, _ = procSetForeground.Call(handle)
}

// alreadyRunning brings an open copy forward instead of starting a second one
// with its own tray icon.
func alreadyRunning() bool {
	name, _ := syscall.UTF16PtrFromString(`Local\PinAICheck`)
	handle, _, err := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	singleInstanceHandle = handle
	if errno, ok := err.(syscall.Errno); !ok || errno != errorAlreadyExists {
		return false
	}
	title, _ := syscall.UTF16PtrFromString(windowTitle)
	if window, _, _ := procFindWindow.Call(0, uintptr(unsafe.Pointer(title))); window != 0 {
		showWindow(window)
	}
	return true
}

// hideInsteadOfClosing sends the close and minimize buttons to the tray; only the
// tray's own exit ends the program.
func hideInsteadOfClosing(window uintptr) {
	hook := syscall.NewCallback(func(hwnd, message, wparam, lparam uintptr) uintptr {
		if message == wmClose || (message == wmSysCommand && wparam&0xFFF0 == scMinimize) {
			_, _, _ = procShowWindow.Call(hwnd, swHide)
			return 0
		}
		result, _, _ := procCallWindowProc.Call(previousWindowProc, hwnd, message, wparam, lparam)
		return result
	})
	previousWindowProc, _, _ = procSetWindowLongPtr.Call(window, gwlpWndProc, hook)
}

// runDesktop is the window and the tray icon. Without the WebView2 runtime (old
// Windows 10 installs) it falls back to the browser page, which still works.
func runDesktop(ctx context.Context, app *relaycheckclient.App, listener net.Listener, url, dataDir string) error {
	view := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:      filepath.Join(dataDir, "webview"),
		AutoFocus:     true,
		WindowOptions: webview2.WindowOptions{Title: windowTitle, Width: 1040, Height: 760, Center: true, IconId: iconResourceID},
	})
	if view == nil {
		openInBrowser(url)
		return app.Serve(ctx, listener)
	}
	window := uintptr(view.Window())
	hideInsteadOfClosing(window)

	app.KeepAlive = true
	served := make(chan error, 1)
	go func() { served <- app.Serve(ctx, listener) }()

	quit := func() {
		view.Dispatch(view.Terminate)
	}
	go runTray(window, quit)

	view.Navigate(url)
	view.Run()
	systray.Quit()
	os.Exit(0)
	return <-served
}

// runTray owns the tray icon. Windows delivers a window's messages only to the
// thread that created it, so the goroutine must stay on one thread: left
// unlocked, the scheduler moves it under load and the icon stops answering
// clicks for good, leaving no way to exit.
func runTray(window uintptr, quit func()) {
	runtime.LockOSThread()
	systray.Run(func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle(windowTitle)
		systray.SetTooltip(windowTitle)
		systray.SetOnTapped(func() { showWindow(window) })
		open := systray.AddMenuItem("打开", "打开窗口")
		exit := systray.AddMenuItem("退出", "退出程序")
		go func() {
			for {
				select {
				case <-open.ClickedCh:
					showWindow(window)
				case <-exit.ClickedCh:
					quit()
					return
				}
			}
		}()
	}, nil)
}
