//go:build !(wasm && js)

package websocket

import (
	"net/http"
	"sync"

	"github.com/blitz-frost/io"
	"github.com/gorilla/websocket"
)

var (
	dialer = websocket.Dialer{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
)

// A Conn wraps a websocket.Conn.
// Is concurrent safe for writing.
// Exposes the websocket.Conn in case any of its non-read/write methods are needed, notably Close.
type Conn struct {
	*websocket.Conn

	// Used to read whole messages from the connection before forwarding them.
	// Should not be changed during an active Listen loop.
	// Text buffer is separate as text messages are likely to need smaller buffers.
	Buffer     io.CascadeWriter
	BufferText io.CascadeWriter

	dst     io.Writer
	dstText io.Writer

	mux sync.Mutex // guard writing
}

func Dial(url string) (*Conn, error) {
	ws, _, err := dialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}

	return &Conn{Conn: ws}, nil
}

func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}

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

// Listen begins a read loop on the underlying websocket, forwarding binary and text messages to their respective chain targets.
// Returns on error.
//
// If custom features are desired, such as memory pooling or message preprocessing, set the Buffer or BufferText members before calling Listen.
// If left nil, simple growing buffers will be used.
func (x *Conn) Listen() error {
	// discard unchained message types
	if x.dst == nil {
		x.dst = io.VoidWriter{}
	}
	if x.dstText == nil {
		x.dstText = io.VoidWriter{}
	}

	// assign default buffers if needed
	if x.Buffer == nil {
		x.Buffer = new(buffer)
	}
	if x.BufferText == nil {
		x.BufferText = new(buffer)
	}

	var (
		buf io.CascadeWriter
		w   io.Writer
		b   = make([]byte, 4096)
	)
	for {
		msgType, r, err := x.NextReader()
		if err != nil {
			return err
		}

		switch msgType {
		case websocket.BinaryMessage:
			buf = x.Buffer
			w = x.dst
		case websocket.TextMessage:
			buf = x.BufferText
			w = x.dstText
		}

		// buffer message
		for {
			n, err := r.Read(b)
			if err != nil {
				break
			}

			if err = buf.Write(b[:n]); err != nil {
				return err
			}
		}

		// forward
		if err = buf.WriteTo(w); err != nil {
			return err
		}
	}

	return nil
}

func (x *Conn) Write(b []byte) error {
	x.mux.Lock()
	err := x.WriteMessage(websocket.BinaryMessage, b)
	x.mux.Unlock()
	return err
}

func (x *Conn) WriteText(b []byte) error {
	x.mux.Lock()
	err := x.WriteMessage(websocket.TextMessage, b)
	x.mux.Unlock()
	return err
}

func (x *Conn) Writer() (io.WriteCloser, error) {
	x.mux.Lock()
	w, err := x.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return nil, err
	}
	return writer{
		w:   w,
		mux: &x.mux,
	}, nil
}

func (x *Conn) WriterText() (io.WriteCloser, error) {
	x.mux.Lock()
	w, err := x.NextWriter(websocket.TextMessage)
	if err != nil {
		return nil, err
	}
	return writer{
		w:   w,
		mux: &x.mux,
	}, nil
}

// A Server wraps a http.ServeMux and http.Server.
// Can be used to set up RPC without having to directly import net/http.
// Advanced use casses can just use the Handler function instead.
type Server struct {
	mux http.ServeMux
	srv http.Server
}

func NewServer(addr string) *Server {
	x := Server{
		mux: *http.NewServeMux(),
	}
	x.srv = http.Server{
		Addr:    addr,
		Handler: &x.mux,
	}

	return &x
}

func (x *Server) Close() error {
	return x.srv.Close()
}

func (x *Server) Handle(path string, h http.Handler) {
	x.mux.Handle(path, h)
}

func (x *Server) HandleWebsocket(path string, setup func(*Conn)) {
	h := Handler(setup)
	x.mux.Handle(path, h)
}

func (x *Server) ListenAndServe() error {
	return x.srv.ListenAndServe()
}

// Handler returns a http.Handler that upgrades incoming http requests to websockets and calls the given setup function with them.
func Handler(setup func(*Conn)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := Upgrade(w, r)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		setup(ws)
	})
}
