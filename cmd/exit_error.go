package cmd

type cliExitError struct {
	msg  string
	code ExitCode
}

func (e *cliExitError) Error() string {
	return e.msg
}

func (e *cliExitError) ExitCode() int {
	if e == nil || e.code == ExitCodeSuccess {
		return int(exitCodeFailure)
	}
	return int(e.code)
}
