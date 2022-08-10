// Package http provides HTTP POST based Processor and Handler implementations for RPC.
package http

import (
	"bytes"
	"errors"
	"net/http"

	"github.com/blitz-frost/rpc"
)

// NewClient returns an rpc.Client that processes its calls via HTTP POST.
// Will send requests to the given url.
func Client(c rpc.Codec, url string) rpc.Client {
	req := rpc.MakeRequester(c, processor(url))
	caller := rpc.MakeCaller(c, req)
	return rpc.MakeClient(caller)
}

// Handler returns a http.Handler that can be added to a http.ServeMux.
// It will process RPC requests.
func Handler(c rpc.Codec, l rpc.Library) http.Handler {
	prc := rpc.MakeProcessor(c, l.AsResponder(c))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		r.Body.Close()

		resp, err := prc.Process(b)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Write(resp)
	})
}

// HandlerCORS wraps h to accept CORS requests from the specified origin.
func HandlerCORS(origin string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			header := w.Header()
			header.Add("access-control-allow-origin", origin)
			header.Add("access-control-allow-method", http.MethodPost)
			header.Add("access-control-allow-headers", "content-type")

			w.Write([]byte("OK"))
		} else {
			w.Header().Add("access-control-allow-origin", origin)
			h.ServeHTTP(w, r)
		}
	})

}

// A Server wraps a http.ServeMux and http.Server.
// Can be used to set up RPC without having to directly import net/http.
// Advanced uses cases can just use Handler instead.
type Server struct {
	mux http.ServeMux
	srv http.Server
}

// NewServer returns an RPC server that will listen on the specified address.
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

// Handle will set up the given Processor to process RPC requests on the specified endpoint.
func (x *Server) HandleRpc(path string, lib rpc.Library, c rpc.Codec) {
	h := Handler(c, lib)
	x.mux.Handle(path, h)
}

func (x *Server) ListenAndServe() error {
	return x.srv.ListenAndServe()
}

type processor string

func (x processor) Process(b []byte) ([]byte, error) {
	resp, err := http.Post(string(x), "application/octet-stream", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("request failed")
	}

	r := make([]byte, resp.ContentLength)
	resp.Body.Read(r)

	return r, nil
}
