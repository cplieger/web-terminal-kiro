// cli_runner.go provides a shared subprocess lifecycle helper that
// eliminates duplication of exec+timeout+kill+error-classify across
// the login/logout/whoami handlers.

package auth

import (
	"context"
	"errors"
	"io/fs"
	"os/exec"
)

// CLIErrorKind classifies subprocess failures into actionable categories.
type CLIErrorKind int

const (
	// CLIErrTimeout indicates the subprocess exceeded its deadline.
	CLIErrTimeout CLIErrorKind = iota + 1
	// CLIErrNotFound indicates the CLI binary is missing.
	CLIErrNotFound
	// CLIErrFailed indicates a generic non-zero exit.
	CLIErrFailed
)

// CLIError is a typed error wrapping a subprocess failure with its
// classification. Handlers use Kind to map to the appropriate HTTP
// status without re-implementing the classification switch.
type CLIError struct {
	Err  error
	Kind CLIErrorKind
}

// classifyCLIError maps a cmd.Run/cmd.Start error + context into a
// CLIError. ctx must be the context used for the command.
func classifyCLIError(ctx context.Context, err error) *CLIError {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &CLIError{Kind: CLIErrTimeout, Err: err}
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return &CLIError{Kind: CLIErrNotFound, Err: err}
	default:
		return &CLIError{Kind: CLIErrFailed, Err: err}
	}
}

// runCLI executes cmd synchronously (cmd.Run) and returns nil on
// success or a classified *CLIError on failure. The caller must have
// already configured cmd via exec.CommandContext with an appropriate
// timeout context. After a timeout, the process group is killed.
func runCLI(ctx context.Context, cmd *exec.Cmd) *CLIError {
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			killLoginProcess(cmd)
		}
		return classifyCLIError(ctx, err)
	}
	return nil
}
