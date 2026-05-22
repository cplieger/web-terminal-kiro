//go:build !unix

package auth

import (
	"errors"
	"os/exec"
)

// setLoginProcAttr is a no-op on non-unix platforms; vibecli runs in
// Linux containers. Tests running on non-unix fall through to the
// single-PID Kill fallback in killLoginProcess.
func setLoginProcAttr(_ *exec.Cmd) {}

// errUnsupportedKill is returned by loginKill on non-unix platforms so
// killLoginProcess falls through to its single-PID Kill fallback.
var errUnsupportedKill = errors.New("process-group kill unsupported on this platform")

// loginKill always returns errUnsupportedKill on non-unix.
func loginKill(_ *exec.Cmd) error { return errUnsupportedKill }
