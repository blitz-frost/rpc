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
func (x *Conn) Listen() error {
	// discard unchained message types
	if x.dst == nil {
		x.dst = io.VoidWriter{}
	}
	if x.dstText == nil {
		x.dstText = io.VoidWriter{}
	}

	for {
		msgType, b, err := x.ReadMessage()
		if err != nil {
			return err
		}

		switch msgType {
		case websocket.BinaryMessage:
			err = x.dst.Write(b)
		case websocket.TextMessage:
			err = x.dstText.Write(b)
		}
		if err != nil {
			return err
		}
	}

	return nil
}

func (x *Conn) Write(b []byte) error {
	x.mux.Lock()
	defer x.mux.Unlock()
	return x.WriteMessage(websocket.BinaryMessage, b)
}

func (x *Conn) WriteText(b []byte) error {
	x.mux.Lock()
	defer x.mux.Unlock()
	return x.WriteMessage(websocket.TextMessage, b)
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
