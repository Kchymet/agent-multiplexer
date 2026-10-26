//go:build darwin

package panespec

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// Every descriptor above stderr must be close-on-exec after the sweep, even
// while descriptors open and close around it.
func TestClosePayloadDescriptorsMarksEveryDescriptor(t *testing.T) {
	var files []*os.File
	for i := 0; i < 32; i++ {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		// Inherited descriptors are not close-on-exec.
		if _, err := unix.FcntlInt(f.Fd(), unix.F_SETFD, 0); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	stop := make(chan struct{})
	churned := make(chan struct{})
	go func() {
		defer close(churned)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if f, err := os.Open(os.DevNull); err == nil {
				_ = f.Close()
			}
		}
	}()
	for i := 0; i < 200; i++ {
		if err := closePayloadDescriptors(); err != nil {
			close(stop)
			<-churned
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	close(stop)
	<-churned
	for _, f := range files {
		flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
		if err != nil {
			t.Fatal(err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("descriptor %d survived the sweep without close-on-exec", f.Fd())
		}
	}
}
