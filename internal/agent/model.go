package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// modelTailBytes bounds work on the daemon's two-second poll path. Both Claude
// and Codex repeat the active model at least once per turn, so the recent tail is
// sufficient without rereading an unbounded conversation on every refresh.
const modelTailBytes int64 = 8 << 20

var modelTailCache = struct {
	sync.Mutex
	byPath map[string]modelTailResult
}{byPath: map[string]modelTailResult{}}

type modelTailResult struct {
	size    int64
	modTime time.Time
	model   string
	ok      bool
}

func latestModelLine(path string, decode func([]byte) string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return "", false
	}
	modelTailCache.Lock()
	if cached, ok := modelTailCache.byPath[path]; ok && cached.size == fi.Size() && cached.modTime.Equal(fi.ModTime()) {
		modelTailCache.Unlock()
		return cached.model, cached.ok
	}
	modelTailCache.Unlock()
	start := fi.Size() - modelTailBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", false
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", false
	}
	if start > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:] // discard the partial first line
		}
	}
	lines := bytes.Split(b, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		if model := strings.TrimSpace(decode(lines[i])); model != "" {
			cacheModelTail(path, fi, model, true)
			return model, true
		}
	}
	cacheModelTail(path, fi, "", false)
	return "", false
}

func cacheModelTail(path string, fi os.FileInfo, model string, ok bool) {
	modelTailCache.Lock()
	modelTailCache.byPath[path] = modelTailResult{size: fi.Size(), modTime: fi.ModTime(), model: model, ok: ok}
	modelTailCache.Unlock()
}

func claudeModelLine(line []byte) string {
	var entry struct {
		Type    string `json:"type"`
		Message struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "assistant" {
		return ""
	}
	return entry.Message.Model
}

func codexModelLine(line []byte) string {
	var entry struct {
		Type    string `json:"type"`
		Payload struct {
			Model string `json:"model"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "turn_context" {
		return ""
	}
	return entry.Payload.Model
}
