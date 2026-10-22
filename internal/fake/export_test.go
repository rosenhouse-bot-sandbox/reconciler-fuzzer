package fake

import "time"

// SetStartTimeout sets startTimeout until the returned func restores it.
func SetStartTimeout(d time.Duration) (restore func()) {
	was := startTimeout
	startTimeout = d
	return func() { startTimeout = was }
}
