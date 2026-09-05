//go:build darwin

package picker

import (
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// inputReady waits only while distinguishing a lone Escape key from the
// beginning of an ANSI key sequence. Regular key reads remain blocking.
func inputReady(reader io.Reader, wait time.Duration) (bool, error) {
	if sized, ok := reader.(interface{ Len() int }); ok {
		return sized.Len() > 0, nil
	}
	file, ok := reader.(*os.File)
	if !ok {
		return true, nil
	}
	timeout := int(wait / time.Millisecond)
	if timeout < 1 {
		timeout = 1
	}
	fds := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, timeout)
	return n > 0, err
}

func enterRawMode(reader io.Reader) (func(), error) {
	file, ok := reader.(*os.File)
	if !ok {
		return func() {}, nil
	}
	fd := int(file.Fd())
	original, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return func() {}, nil
	}
	raw := *original
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, &raw); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TIOCSETA, original) }, nil
}
