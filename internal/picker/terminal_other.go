//go:build !darwin

package picker

import (
	"io"
	"time"
)

func enterRawMode(io.Reader) (func(), error) { return func() {}, nil }

func inputReady(reader io.Reader, _ time.Duration) (bool, error) {
	if sized, ok := reader.(interface{ Len() int }); ok {
		return sized.Len() > 0, nil
	}
	return true, nil
}

func terminalWidth(io.Writer) int { return 0 }
