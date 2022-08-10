// Package json provides an rpc.Codec implementation based on the standard library encoding/json.
package json

import (
	"bytes"
	"encoding/json"
	"reflect"

	"github.com/blitz-frost/rpc"
)

type Codec struct{}

func (x Codec) Decoder() rpc.Decoder {
	return &Decoder{}
}

func (x Codec) Encoder() rpc.Encoder {
	return NewEncoder()
}

type Decoder struct {
	dec *json.Decoder
}

func (x Decoder) Decode(v any) error {
	return x.dec.Decode(v)
}

func (x Decoder) DecodeValues(v []reflect.Value) error {
	for _, val := range v {
		if err := x.dec.Decode(val.Interface()); err != nil {
			return err
		}
	}
	return nil
}

func (x *Decoder) Write(b []byte) error {
	buf := bytes.NewReader(b)
	x.dec = json.NewDecoder(buf)
	return nil
}

type Encoder struct {
	vals []reflect.Value
	ord  []any // hold values to be encoded, in the order they were given
	buf  *bytes.Buffer
	enc  *json.Encoder
}

func NewEncoder() *Encoder {
	buf := &bytes.Buffer{}
	return &Encoder{
		buf: buf,
		enc: json.NewEncoder(buf),
	}
}

func (x *Encoder) Encode(v any) error {
	x.ord = append(x.ord, v)
	return nil
}

func (x *Encoder) EncodeValues(v []reflect.Value) error {
	x.vals = v
	return nil
}

func (x Encoder) Read() ([]byte, error) {
	// encoded normal values in reverse order
	for i := len(x.ord) - 1; i >= 0; i-- {
		if err := x.enc.Encode(x.ord[i]); err != nil {
			return nil, err
		}
	}

	// reflect values are always encoded last
	for _, v := range x.vals {
		if err := x.enc.Encode(v.Interface()); err != nil {
			return nil, err
		}
	}
	return x.buf.Bytes(), nil
}
