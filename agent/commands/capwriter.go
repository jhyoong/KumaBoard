package commands

import (
	"sync"
	"unicode/utf8"

	"github.com/jhyoong/KumaBoard/proto"
)

// MaxOutput is how many bytes of stdout or stderr are retained: the newest
// ones. Earlier output is streamed while the command runs and then gone.
const MaxOutput = proto.MaxCommandOutput

// capWriter is a ring buffer holding the newest MaxOutput bytes written to
// it, plus the total ever written so a flusher can tell what it has not sent
// yet. It never blocks or fails the writer, so a chatty command is not slowed
// down by a slow connection.
type capWriter struct {
	mu    sync.Mutex
	buf   []byte // the last len(buf) bytes written, oldest first
	total int64  // bytes ever written
	sent  int64  // offset up to which next has handed bytes out
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += int64(len(p))
	if len(p) >= MaxOutput {
		w.buf = append(w.buf[:0], p[len(p)-MaxOutput:]...)
		return len(p), nil
	}
	if over := len(w.buf) + len(p) - MaxOutput; over > 0 {
		w.buf = w.buf[:copy(w.buf, w.buf[over:])]
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// String returns the retained tail.
func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// truncated reports whether anything was dropped from the front.
func (w *capWriter) truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total > int64(len(w.buf))
}

// next returns the bytes written since the previous call. If more arrived
// than the ring holds, only the newest are returned and skipped is true.
// Unless final, an incomplete UTF-8 sequence at the end is held back for the
// next call so a multi-byte character is never split across two chunks.
func (w *capWriter) next(final bool) (data string, skipped bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	start := w.total - int64(len(w.buf)) // offset of buf[0]
	from := w.sent
	if from < start {
		from, skipped = start, true
	}
	chunk := w.buf[from-start:]
	if skipped {
		// The cut may have landed inside a character; drop its remainder.
		for i := 0; i < utf8.UTFMax-1 && len(chunk) > 0 && !utf8.RuneStart(chunk[0]); i++ {
			chunk = chunk[1:]
			from++
		}
	}
	if !final {
		chunk = chunk[:len(chunk)-incompleteSuffix(chunk)]
	}
	w.sent = from + int64(len(chunk))
	return string(chunk), skipped
}

// incompleteSuffix is the length of a trailing, so far valid but unfinished,
// UTF-8 sequence in b: 0 to 3 bytes.
func incompleteSuffix(b []byte) int {
	for n := 1; n < utf8.UTFMax && n <= len(b); n++ {
		c := b[len(b)-n]
		if utf8.RuneStart(c) {
			if c >= utf8.RuneSelf && !utf8.FullRune(b[len(b)-n:]) {
				return n
			}
			return 0
		}
	}
	return 0
}
