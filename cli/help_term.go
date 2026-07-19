package cli

import (
	"io"
	"os"

	"golang.org/x/term"
)

type termInfo struct {
	width int
	color bool
}

const defaultWidth = 80

// detectTermInfo enables terminal width and colour only for terminal files, respecting NO_COLOR.
func detectTermInfo(w io.Writer) termInfo {
	info := termInfo{width: defaultWidth}
	f, ok := w.(*os.File)
	if !ok {
		return info
	}
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return info
	}
	if cols, _, err := term.GetSize(fd); err == nil && cols > 0 {
		info.width = cols
	}
	if os.Getenv("NO_COLOR") == "" {
		info.color = true
	}
	return info
}

func (t termInfo) bold(s string) string {
	if !t.color {
		return s
	}
	return "\033[1m" + s + "\033[0m"
}
