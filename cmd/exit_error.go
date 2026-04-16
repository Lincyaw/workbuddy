package cmd

type cliExitError struct {
	msg  string
	code int
}

func (e *cliExitError) Error() string {
	return e.msg
}

func (e *cliExitError) ExitCode() int {
	if e == nil || e.code == 0 {
		return 1
	}
	return e.code
}
