package tailnet

// SetGOOS replaces the platform the package reports messages for.
func SetGOOS(g string) (restore func()) {
	old := goos
	goos = g
	return func() { goos = old }
}

// Classify exposes classify.
func Classify(err error, socket string) error { return classify(err, socket) }
