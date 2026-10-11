package cli

const (
	exitOK      = 0
	exitFailure = 1
	exitBlocked = 2
	exitInvalid = 3

	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiGray   = "\x1b[90m"

	envNoColor = "NO_COLOR"

	logLevelDebug = "debug"
	logLevelInfo  = "info"
	logLevelWarn  = "warn"
	logLevelError = "error"

	jsonIndent = "  "

	// allowHostFlag is still accepted so older compose commands keep working.
	allowHostFlag = "allow-host"

	// Asked right under the plan table, which lists the images. Teardown is
	// down -v without --rmi, so the images stay for the real deployment.
	sandboxQuestion = "\nRun %s in an isolated sandbox (pulls the images above and keeps them; the sandbox and its volumes are removed afterwards)?"
)
