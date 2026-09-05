//go:build !darwin

package picker

import "io"

func enterRawMode(io.Reader) (func(), error) { return func() {}, nil }
