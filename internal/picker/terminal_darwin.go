//go:build darwin

package picker

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

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
