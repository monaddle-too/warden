// Package services holds what the four Warden services share now that they
// are subcommands of the one warden binary (warden policy, warden runner,
// warden serve, warden edge): the exit-status convention and flag parsing.
// Each service package exposes Main(args) int, which the warden command
// dispatches to; the services still run as separate processes with
// separate privileges, only the executable is shared.
package services

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
)

// ExitCode is an error carrying the process exit status a service wants
// (0 for a completed one-shot subcommand, 2 for a usage error).
type ExitCode int

func (e ExitCode) Error() string { return "exit status " + strconv.Itoa(int(e)) }

// ParseFlags parses args and maps -h to exit status 0 and any other flag
// error (already printed by the flag package) to status 2.
func ParseFlags(fs *flag.FlagSet, args []string) error {
	switch err := fs.Parse(args); {
	case err == nil:
		return nil
	case errors.Is(err, flag.ErrHelp):
		return ExitCode(0)
	default:
		return ExitCode(2)
	}
}

// Run turns a service's run function into an exit status: nil is 0, an
// ExitCode is itself, anything else is printed to stderr and is 1.
func Run(run func(args []string) error, args []string) int {
	err := run(args)
	var code ExitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	default:
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
}
