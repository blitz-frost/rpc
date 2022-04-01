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

type signaler struct {
	fnCandidate func(string) error
	fnSdp       func(webrtc.SessionDescription) error
}

func (x signaler) candidate(candidate *webrtc.ICECandidate) error {
	arg := candidate.ToJSON().Candidate
	return x.fnCandidate(arg)
}

func (x *signaler) setup(conn *webrtc.PeerConnection, cw io.ChainWriter, answerFunc func() error) {
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
			go x.candidate(candidate)
		}
		mux.Unlock()

		return nil
	})

	gate := rpc.NewDualGate(json.Codec{}, lib.AsResponder(json.Codec{}), cw, nil)
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
			go x.candidate(candidate)
		}
	})
}

func SignalAnswer(conn *webrtc.PeerConnection, cw io.ChainWriter) error {
	sig := signaler{}
	answerFunc := func() error {
		answer, err := conn.CreateAnswer(nil)
		if err != nil {
			return err
		}

		go sig.fnSdp(answer)

		return conn.SetLocalDescription(answer)
	}
	sig.setup(conn, cw, answerFunc)

	return nil
}

func SignalOffer(conn *webrtc.PeerConnection, cw io.ChainWriter) error {
	offer, err := conn.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err = conn.SetLocalDescription(offer); err != nil {
		return err
	}

	sig := signaler{}
	answerFunc := func() error { return nil }

	sig.setup(conn, cw, answerFunc)

	return sig.fnSdp(offer)
}
