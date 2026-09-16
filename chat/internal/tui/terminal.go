package tui

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// RunOnTerminal puts stdin in raw mode, runs the app on the real terminal and
// restores the terminal afterwards, whatever happened.
func RunOnTerminal(ctx context.Context, app *App) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("warden chat needs an interactive terminal; use `warden chat send` or `warden chat list` from scripts")
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)
	app.Input = os.Stdin
	app.Output = os.Stdout
	app.Size = func() (int, int) {
		w, h, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			return 100, 30
		}
		return w, h
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	resize := make(chan struct{}, 1)
	go func() {
		for range winch {
			select {
			case resize <- struct{}{}:
			default:
			}
		}
	}()
	app.Resize = resize
	return app.Run(ctx)
}
