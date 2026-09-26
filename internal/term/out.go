package term

import (
	"fmt"
	"io"
)

// Out writes a host command's progress: steps, plain lines, and warnings. It never carries secrets; commands that
// handle them never pass them here.
type Out struct {
	W   io.Writer
	Err io.Writer
}

// Step starts a section of the output.
func (o *Out) Step(format string, args ...any) {
	_, _ = fmt.Fprintf(o.W, "\n==> "+format+"\n", args...)
}

// Say writes one line.
func (o *Out) Say(format string, args ...any) {
	_, _ = fmt.Fprintf(o.W, format+"\n", args...)
}

// Warn writes a warning to the error stream.
func (o *Out) Warn(format string, args ...any) {
	_, _ = fmt.Fprintf(o.Err, "warning: "+format+"\n", args...)
}
