package main

import (
	"os"

	"amux/internal/harness"
	"amux/internal/harnessproto"
	"amux/internal/mux"
)

// cmdServe runs the multiplexer server (the backend). Extra args are additional
// listen specs, e.g. `amux serve tcp:0.0.0.0:7077` to allow remote UIs.
func cmdServe(args []string) error { return mux.Run(args...) }

// cmdHarness runs the agent harness over stdio — spawned by the server (or run
// manually / over ssh) to own agent pane processes on this machine.
func cmdHarness() error {
	return harness.Serve(harnessproto.NewConn(stdio{}))
}

// stdio adapts the process's stdin/stdout to one io.ReadWriteCloser.
type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return nil }
