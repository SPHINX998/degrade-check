//go:build !windows

package main

import (
	"context"
	"net"

	"github.com/SPHINX998/degrade-check/internal/features/relaycheckclient"
)

// macOS and Linux keep the browser page: a tray needs cgo, which cannot be cross
// built from where releases are made.
func runDesktop(ctx context.Context, app *relaycheckclient.App, listener net.Listener, url, _ string) error {
	openInBrowser(url)
	return app.Serve(ctx, listener)
}

func alreadyRunning() bool { return false }
