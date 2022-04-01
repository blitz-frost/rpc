//go:build wasm && js

package websocket

import (
	"errors"
	"strconv"
	"sync"

	"github.com/blitz-frost/io"
	"github.com/blitz-frost/wasm/websocket"
)

type Conn struct {
	*websocket.Conn

	dst     io.Writer
	dstText io.Writer

	// unclear if this is needed in wasm
	mux sync.Mutex
}

func Dial(url string) (*Conn, error) {
	ws := websocket.Dial(url)
	return &Conn{Conn: ws}, nil
}

func (x *Conn) AsText() io.ChainWriter {
	return chainWriter{
		chain:    x.ChainText,
		chainGet: x.ChainGetText,
		write:    x.WriteText,
	}
}

func (x *Conn) Chain(w io.Writer) {
	x.dst = w
}

func (x Conn) ChainGet() io.Writer {
	return x.dst
}

func (x *Conn) ChainText(w io.Writer) {
	x.dstText = w
}

func (x Conn) ChainGetText() io.Writer {
	return x.dstText
}

// Listen exists for consistency with the non-wasm version.
// Since the wasm websocket is handler based instead of read based, a Listen method isn't technically needed.
func (x *Conn) Listen() error {
	if x.dst == nil {
		x.dst = io.VoidWriter{}
	}
	if x.dstText == nil {
		x.dstText = io.VoidWriter{}
	}

	// get a single error without blocking handlers
	mux := sync.Mutex{}
	done := false
	ch := make(chan error)
	errFunc := func(err error) {
		mux.Lock()
		defer mux.Unlock()
		if done {
			return
		}
		ch <- err
		done = true
	}

	x.Conn.OnClose(func(c websocket.Code) {
		var err error
		if c != websocket.CodeNormal {
			err = errors.New("websocket closed: " + strconv.Itoa(int(c)))
		}
		errFunc(err)
	})
	x.Conn.OnBinary(func(b []byte) {
		if err := x.dst.Write(b); err != nil {
			errFunc(err)
		}
	})
	x.Conn.OnText(func(s string) {
		if err := x.dstText.Write([]byte(s)); err != nil {
			errFunc(err)
		}
	})

	return <-ch
}

func (x *Conn) Write(b []byte) error {
	x.mux.Lock()
	defer x.mux.Unlock()
	return x.Conn.Write(b)
}

func (x *Conn) WriteText(b []byte) error {
	x.mux.Lock()
	defer x.mux.Unlock()
	return x.Conn.WriteText(b)
}
