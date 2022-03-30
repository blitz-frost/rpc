package websocket

import (
	"github.com/blitz-frost/io"
)

type chainWriter struct {
	chain    func(io.Writer)
	chainGet func() io.Writer
	write    func([]byte) error
}

func (x chainWriter) Chain(w io.Writer) {
	x.chain(w)
}

func (x chainWriter) ChainGet() io.Writer {
	return x.chainGet()
}

func (x chainWriter) Write(b []byte) error {
	return x.write(b)
}
