package websocket

import (
	"github.com/blitz-frost/io"
)

// buffer is the default io.CascadeWriter used to read messages internally.
type buffer []byte

func (x *buffer) Write(b []byte) error {
	*x = append(*x, b...)
	return nil
}

func (x *buffer) WriteTo(w io.Writer) error {
	err := w.Write(*x)
	*x = (*x)[:0]
	return err
}

// chainWriter is used to convert a connection to a text io.ChainWriter
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
