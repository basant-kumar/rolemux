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
	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		timeout := int((remaining + time.Millisecond - 1) / time.Millisecond)
		fds := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, timeout)
		if err == unix.EINTR {
			continue
		}
		return n > 0, err
	}
}

func terminalWidth(writer io.Writer) int {
	file, ok := writer.(*os.File)
	if !ok {
		return 0
	}
	size, err := unix.IoctlGetWinsize(int(file.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(size.Col)
}

func terminalHeight(writer io.Writer) int {
	file, ok := writer.(*os.File)
	if !ok {
		return 0
	}
	size, err := unix.IoctlGetWinsize(int(file.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(size.Row)
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
