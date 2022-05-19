package websocket

import (
	stdio "io"
	"sync"

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

// A writer is used to write fragmented data to a connection, instead of full byte slices.
type writer struct {
	w   stdio.WriteCloser
	mux *sync.Mutex // owning websocket write guard
}

func (x writer) Close() error {
	err := x.w.Close()
	x.mux.Unlock()
	return err
}

func (x writer) Write(b []byte) error {
	_, err := x.w.Write(b)
	return err
}
