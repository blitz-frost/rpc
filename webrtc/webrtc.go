package webrtc

import (
	"sync"

	"github.com/blitz-frost/io"
	"github.com/blitz-frost/rpc"
	"github.com/blitz-frost/rpc/json"
	"github.com/pion/webrtc/v3"
)

type Channel struct {
	ch *webrtc.DataChannel
	w  io.Writer
}

func NewChannel(ch *webrtc.DataChannel) *Channel {
	x := Channel{
		ch: ch,
		w:  io.VoidWriter{},
	}
	ch.OnMessage(func(msg webrtc.DataChannelMessage) {
		x.w.Write(msg.Data)
	})
	return &x
}

func (x *Channel) Chain(w io.Writer) {
	x.w = w
}

func (x Channel) ChainGet() io.Writer {
	return x.w
}

func (x Channel) Close() error {
	return x.ch.Close()
}

func (x Channel) OnClose(fn func()) {
	x.ch.OnClose(fn)
}

/*
Not present in wasm version
func (x Channel) OnError(fn func(error)) {
	x.ch.OnError(fn)
}
*/

func (x Channel) OnOpen(fn func()) {
	x.ch.OnOpen(fn)
}

func (x Channel) Write(b []byte) error {
	return x.ch.Send(b)
}

type Signaler struct {
	fnCandidate func(string) error
	fnSdp       func(webrtc.SessionDescription) error
}

func (x Signaler) candidate(candidate *webrtc.ICECandidate) error {
	arg := candidate.ToJSON().Candidate
	return x.fnCandidate(arg)
}

func (x *Signaler) setup(conn *webrtc.PeerConnection, cw io.ChainWriter, answerFunc func() error) {
	pending := make([]*webrtc.ICECandidate, 0)
	mux := sync.Mutex{}

	// prepare rpc
	lib := rpc.MakeLibrary()
	lib.Register("candidate", func(s string) error {
		return conn.AddICECandidate(webrtc.ICECandidateInit{Candidate: s})
	})
	lib.Register("sdp", func(sdp webrtc.SessionDescription) error {
		if err := conn.SetRemoteDescription(sdp); err != nil {
			return err
		}

		if err := answerFunc(); err != nil {
			return err
		}

		mux.Lock()
		for _, candidate := range pending {
			x.candidate(candidate)
		}
		mux.Unlock()

		return nil
	})

	gate := rpc.NewDualGate(json.Codec{}, lib.AsResponder(json.Codec{}), cw, nil)
	// in wasm environment data is likely delivered by a JS callback, we must not block that
	// the write call itself should never be returning an error anyway
	cw.Chain(io.WriterFunc(func(b []byte) error {
		go gate.Write(b)
		return nil
	}))
	caller := rpc.MakeCaller(json.Codec{}, gate)
	cli := rpc.MakeClient(caller)

	cli.Bind("candidate", &x.fnCandidate)
	cli.Bind("sdp", &x.fnSdp)

	conn.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		mux.Lock()
		defer mux.Unlock()

		desc := conn.RemoteDescription()
		if desc == nil {
			pending = append(pending, candidate)
		} else {
			x.candidate(candidate)
		}
	})
}

func SignalAnswer(conn *webrtc.PeerConnection, cw io.ChainWriter) error {
	sig := Signaler{}
	answerFunc := func() error {
		answer, err := conn.CreateAnswer(nil)
		if err != nil {
			return err
		}

		sig.fnSdp(answer)

		return conn.SetLocalDescription(answer)
	}
	sig.setup(conn, cw, answerFunc)

	return nil
}

// SignalOffer starts the WebRTC signaling process for a peer connection, using the provided data carrier.
// Returns a function that can be used for renegotiation.
func SignalOffer(conn *webrtc.PeerConnection, cw io.ChainWriter) (func() error, error) {
	sig := Signaler{}
	answerFunc := func() error { return nil }

	sig.setup(conn, cw, answerFunc)

	fn := func() error {
		offer, err := conn.CreateOffer(nil)
		if err != nil {
			return err
		}
		if err = conn.SetLocalDescription(offer); err != nil {
			return err
		}

		err = sig.fnSdp(offer)

		return err
	}

	return fn, fn()
}
