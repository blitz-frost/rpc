package ice

import (
	"reflect"

	"github.com/blitz-frost/ice"
	"github.com/blitz-frost/rpc"
)

type Codec struct{}

func (x Codec) Decoder() rpc.Decoder {
	return &Decoder{}
}

func (x Codec) Encoder() rpc.Encoder {
	return &Encoder{}
}

type Decoder struct {
	block ice.Block
}

func (x Decoder) Decode(v any) error {
	return x.block.Thaw(v)
}

func (x Decoder) DecodeValues(v []reflect.Value) error {
	return x.block.Thaw(&v)
}

func (x *Decoder) Write(b []byte) error {
	x.block = ice.FromBytes(b)
	return nil
}

type Encoder struct {
	vals []reflect.Value
	ord  []any
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
	block := ice.NewBlock()
	for i := len(x.ord) - 1; i >= 0; i-- {
		if err := block.Freeze(x.ord[i]); err != nil {
			return nil, err
		}
	}

	if err := block.Freeze(x.vals); err != nil {
		return nil, err
	}

	return block.ToBytes(), nil
}
