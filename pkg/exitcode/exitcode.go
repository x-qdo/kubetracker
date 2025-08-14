package exitcode

// Exit codes:
//   0: All tracked resources reached the desired state.
//   1: One or more resources failed.
//   2: Timed out.
//   3: Invalid input (bad YAML, missing kube context, etc.).

// Constants for well-known exit codes used by the CLI.
const (
	ExitOK           = 0
	ExitFailed       = 1
	ExitTimedOut     = 2
	ExitInvalidInput = 3
)

// ExitError carries an explicit process exit code along with an underlying error.
// It implements the error interface and supports errors.Unwrap.
type ExitError struct {
	Code int   // process exit code
	Err  error // underlying error (optional)
}

func (e *ExitError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

// Unwrap allows errors.Is / errors.As to inspect the underlying error.
func (e *ExitError) Unwrap() error { return e.Err }

// New constructs a new ExitError with the given code and underlying error.
func New(code int, err error) *ExitError {
	return &ExitError{Code: code, Err: err}
}

// InvalidInput wraps an error to signal exit code 3 (invalid input).
func InvalidInput(err error) error {
	return New(ExitInvalidInput, err)
}

// Timeout wraps an error to signal exit code 2 (timeout).
func Timeout(err error) error {
	return New(ExitTimedOut, err)
}
