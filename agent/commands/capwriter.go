package commands

import "bytes"

// MaxOutput is the maximum number of bytes captured from stdout or stderr.
const MaxOutput = 64 << 10

type capWriter struct {
	buf       bytes.Buffer
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := MaxOutput - w.buf.Len()
	if room <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *capWriter) String() string { return w.buf.String() }
