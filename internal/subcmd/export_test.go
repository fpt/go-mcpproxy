package subcmd

// SetOpenBrowser replaces the browser opener for tests.
func SetOpenBrowser(fn func(string) error) func() {
	prev := openBrowser
	openBrowser = fn
	return func() { openBrowser = prev }
}

var (
	SplitCallArgs      = splitCallArgs
	ParseToolArguments = parseToolArguments
)
