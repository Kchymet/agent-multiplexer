package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type memoryRWC struct {
	*bytes.Reader
}

func (m *memoryRWC) Write(p []byte) (int, error) { return len(p), nil }
func (m *memoryRWC) Close() error                { return nil }

func TestReadDoesNotConsumeFollowingFrame(t *testing.T) {
	rw := &memoryRWC{Reader: bytes.NewReader([]byte("{\"n\":1}\n{\"n\":2}\n"))}
	c := New(rw)
	for want := 1; want <= 2; want++ {
		var got struct {
			N int `json:"n"`
		}
		if err := c.Read(&got); err != nil {
			t.Fatal(err)
		}
		if got.N != want {
			t.Fatalf("frame %d decoded n=%d", want, got.N)
		}
	}
}

func TestReadRejectsOversizedFrame(t *testing.T) {
	data := bytes.Repeat([]byte{'x'}, MaxFrameBytes+1)
	rw := &memoryRWC{Reader: bytes.NewReader(data)}
	var got any
	if err := New(rw).Read(&got); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized read error = %v, want %v", err, ErrFrameTooLarge)
	}
	if _, err := io.Copy(io.Discard, rw); err != nil {
		t.Fatal(err)
	}
}
