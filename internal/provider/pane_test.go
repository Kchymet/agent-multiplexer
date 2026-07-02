package provider

import (
	"bytes"
	"testing"
)

// TestPaneBufReplay covers the core replay contract: seqs are monotonic from 1,
// framesAfter returns exactly the frames past a cursor, and an unseen prefix that
// was never trimmed needs no reset.
func TestPaneBufReplay(t *testing.T) {
	var b paneBuf
	b.appendOutput([]byte("AAA"))
	b.appendOutput([]byte("BBB"))
	b.appendOutput([]byte("CCC"))

	needReset, _, out, last := b.framesAfter(0)
	if needReset {
		t.Fatal("full replay of an untrimmed buffer must not reset")
	}
	if last != 3 || len(out) != 3 {
		t.Fatalf("framesAfter(0) = %d frames, last %d", len(out), last)
	}
	if out[0].seq != 1 || out[2].seq != 3 {
		t.Fatalf("seqs = %d..%d, want 1..3", out[0].seq, out[2].seq)
	}

	// From afterSeq=1 we expect only BBB, CCC.
	_, _, out, last = b.framesAfter(1)
	var got []byte
	for _, f := range out {
		got = append(got, f.data...)
	}
	if !bytes.Equal(got, []byte("BBBCCC")) || last != 3 {
		t.Fatalf("framesAfter(1) = %q last %d, want BBBCCC/3", got, last)
	}
}

// TestPaneBufTrimResets proves that overflowing the cap trims to the keep-tail
// and that a reader whose next wanted frame was dropped gets a reset pointing at
// the oldest retained seq — bounded memory, never a silent gap.
func TestPaneBufTrimResets(t *testing.T) {
	var b paneBuf
	chunk := bytes.Repeat([]byte("x"), 128<<10) // 128 KiB per frame
	// Push well past replayCap (4 MiB) so a trim to replayKeep (256 KiB) fires.
	for i := 0; i < 40; i++ {
		b.appendOutput(chunk)
	}
	// The buffer refills up to replayCap between trims, so the invariant is a
	// hard ceiling at the cap (not the keep-tail, which only holds momentarily
	// right after a trim fires).
	if b.bytes > replayCap {
		t.Fatalf("buffer exceeded cap: %d bytes > %d", b.bytes, replayCap)
	}

	// A reader still at seq 0 can't get everything: it must be reset to the oldest
	// retained frame.
	needReset, resetSeq, out, _ := b.framesAfter(0)
	if !needReset {
		t.Fatal("expected a reset after trim")
	}
	oldest := out[0].seq
	if resetSeq != oldest {
		t.Fatalf("reset seq %d != oldest retained %d", resetSeq, oldest)
	}
	if oldest <= 1 {
		t.Fatalf("nothing was trimmed (oldest seq %d)", oldest)
	}

	// A reader already past the trim boundary gets no reset.
	needReset, _, _, _ = b.framesAfter(b.seq - 1)
	if needReset {
		t.Fatal("a reader within the retained window must not be reset")
	}
}
