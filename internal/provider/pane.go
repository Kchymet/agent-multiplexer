package provider

import (
	"os"
	"os/exec"
	"sync"
)

// frame kinds in a pane's replay log. reset is not stored as a frame — it is
// synthesised from a trim (its seq is the oldest retained frame's seq), so the
// stored frames stay contiguous in seq.
const (
	frameOutput = iota
	frameExit
)

// frame is one entry in a pane's replay log. Every output and exit frame
// consumes exactly one per-pane seq (monotonic from 1), so stored seqs are
// contiguous and framesAfter can index directly.
type frame struct {
	seq  int64
	kind int
	data []byte // frameOutput
	err  string // frameExit
}

const (
	// replayCap is the per-pane replay-buffer ceiling. Beyond it a consumer that
	// disconnected is too far behind to resync losslessly, so we trim to the tail
	// and flag a reset. Matches docs/remote-provider.md (mirrors internal/daemon
	// and internal/mux, the other lossless two-path writers).
	replayCap = 4 << 20
	// replayKeep is the recent tail retained on a trim. A full-screen agent
	// repaints within this window, so after a reset the consumer rebuilds from it.
	replayKeep = 256 << 10
)

// paneBuf is a pane's authoritative replay log: an ordered, seq-numbered history
// of output/exit frames that survives connection loss so a reconnecting
// orchestrator can replay from any afterSeq. It is bounded (replayCap): on
// overflow the oldest frames are dropped to replayKeep and framesAfter reports a
// reset to the next reader. All access is under mu; callers copy frames out and
// release the lock before writing to a (possibly blocking) socket, so the PTY
// pump that appends here never stalls behind a slow orchestrator.
type paneBuf struct {
	mu     sync.Mutex
	seq    int64
	frames []frame
	bytes  int // sum of output frame data lengths, for the cap
	exited bool
}

// appendOutput records a PTY read as one output frame with the next seq. data is
// copied (the pump reuses its read buffer).
func (b *paneBuf) appendOutput(data []byte) {
	d := make([]byte, len(data))
	copy(d, data)
	b.mu.Lock()
	b.seq++
	b.frames = append(b.frames, frame{seq: b.seq, kind: frameOutput, data: d})
	b.bytes += len(d)
	b.trim()
	b.mu.Unlock()
}

// appendExit records the pane's terminal exit frame; nothing follows it.
func (b *paneBuf) appendExit(errMsg string) {
	b.mu.Lock()
	b.seq++
	b.frames = append(b.frames, frame{seq: b.seq, kind: frameExit, err: errMsg})
	b.exited = true
	b.mu.Unlock()
}

// trim drops the oldest frames once the buffer exceeds replayCap, down to the
// replayKeep tail. It reallocates the retained frames into a fresh slice so the
// multi-MiB backing array is released. Caller holds b.mu.
func (b *paneBuf) trim() {
	if b.bytes <= replayCap {
		return
	}
	i := 0
	for b.bytes > replayKeep && i < len(b.frames)-1 {
		b.bytes -= len(b.frames[i].data)
		i++
	}
	kept := make([]frame, len(b.frames)-i)
	copy(kept, b.frames[i:])
	b.frames = kept
}

// framesAfter returns the frames a reader that has seen up to sent still needs.
// If the next wanted frame (sent+1) was trimmed away, needReset is set with
// resetSeq = the oldest retained seq: the reader must clear its emulator, then
// apply out (which starts at resetSeq). last is the seq the reader will have seen
// after applying out. Frames are copied so the caller can write them without
// holding the lock.
func (b *paneBuf) framesAfter(sent int64) (needReset bool, resetSeq int64, out []frame, last int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	last = sent
	if len(b.frames) == 0 {
		return false, 0, nil, last
	}
	oldest := b.frames[0].seq
	start := sent + 1
	if start < oldest {
		needReset, resetSeq, start = true, oldest, oldest
	}
	idx := int(start - oldest)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(b.frames) {
		return needReset, resetSeq, nil, last
	}
	out = make([]frame, len(b.frames)-idx)
	copy(out, b.frames[idx:])
	last = out[len(out)-1].seq
	return needReset, resetSeq, out, last
}

// snapshot reports the last seq emitted and whether the pane has exited, for
// building a register resume offer.
func (b *paneBuf) snapshot() (lastSeq int64, exited bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq, b.exited
}

// pane is a running (or recently-exited) PTY-backed process plus its replay
// buffer. Unlike the v1 harness, a pane outlives the connection: on disconnect it
// keeps running and buffering within the grace window (see Provider).
type pane struct {
	ptmx *os.File
	cmd  *exec.Cmd
	buf  *paneBuf
}

// terminate kills the process and closes the PTY. Safe on an already-exited pane
// (ptmx may be nil or closed).
func (p *pane) terminate() {
	if p.ptmx != nil {
		_ = p.ptmx.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}
