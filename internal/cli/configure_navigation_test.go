package cli

import "io"

// configureEscapeReader places a real sequence boundary after Escape. Select
// waits for that boundary when distinguishing a lone Escape from an ANSI key
// sequence; a bytes.Buffer would let it consume the next scripted key as the
// probe byte.
type configureEscapeReader struct {
	chunks [][]byte
	index  int
}

func (r *configureEscapeReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.index]
	r.index++
	if len(chunk) == 0 {
		return 0, nil
	}
	return copy(p, chunk), nil
}
