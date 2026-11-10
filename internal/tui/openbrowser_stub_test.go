package tui

import "sync"

// No test in this package may open a real browser: openBrowser is replaced for
// the whole test binary, and the URLs it was asked for are recorded.
var (
	openedURLsMu sync.Mutex
	openedURLs   []string
)

func init() {
	openBrowser = func(url string) {
		openedURLsMu.Lock()
		openedURLs = append(openedURLs, url)
		openedURLsMu.Unlock()
	}
}
