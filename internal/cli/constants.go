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

	// optionalServiceLine and optionalLabel show a service upstream starts
	// only with a Compose profile, switched on with enableOptionalFlag.
	optionalServiceLine = "%s %s (upstream profile %s): --%s %s\n"
	optionalLabel       = "optional"
	enableOptionalFlag  = "enable-optional"
	// composeFileFlag names the upstream compose file to build from.
	composeFileFlag = "compose-file"
	// optionalServiceKind and optionalServiceFact list such a service
	// among the facts.
	optionalServiceKind = "optional service"
	optionalServiceFact = "%s (profile %s)"

	// allowHostFlag is still accepted so older compose commands keep working.
	allowHostFlag = "allow-host"

	// Asked right under the plan table, which lists the images. Teardown is
	// down -v without --rmi, so the images stay for the real deployment.
	sandboxQuestion = "\nRun %s in an isolated sandbox (pulls the images above and keeps them; the sandbox and its volumes are removed afterwards)?"

	// The repair a model proposes after a failed run: the heading, its
	// lines, and the question that applies it.
	proposalHeading = "proposed repair of %s by %s; nothing is stored yet\n"
	addedMark       = "  + "
	removedMark     = "  - "
	leftOutPrefix   = "left out: "
	notePrefix      = "note: "
	gateClear       = "policy gate: passed"
	gateBlocked     = "policy gate: blocks it"
	repairQuestion  = "\nStore this repair as version %d and run the test again?"
	noModelHint     = "; choose one with: composewright config set llm.provider <provider>"
)
