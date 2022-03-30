/*
Package rpc provides bridging of function calls between two Go programs.

A good approach when using this package is to write a common "cross" package that defines one or more Interfaces.
That package would then be important by all involved programs.
An example such package would look like this:

	package cross

	import "github.com/blitz-frost/rpc"

	var (
		Func1 funcType1
		Func2 funcType2
		...
	)

	var AnInterface = rpc.Interface{
		Declare: map[string]any{
			name1: &Func1,
			name2: &Func2,
			...
		},
	}

The server side would then look something like this:

	import (
		"github.com/blitz-frost/rpc"
		"path/to/cross"
	)

	func init() {
		cross.AnInterface.Define = map[string]any{
			name1: localFunc1,
			name2: localFunc2,
			...
		}

		lib := rpc.MakeLibrary()
		cross.AnInterface.RegisterWith(lib)
		... make lib available ...
	}

The client side:

	import (
		"github.com/blitz-frost/rpc"
		"path/to/cross"
	)

	func init() {
		var cli rpc.Client
		... client setup ...
		cross.AnInterface.BindTo(cli)
	}

	func usage() {
		ret1 := cross.Func1(args1)
		ret2 := cross.Func2(args2)
		...
	}
*/
package rpc

import (
	"errors"
	"reflect"
	"sync"

	"github.com/blitz-frost/io"
)

// A bit of a shenanigan in order to always have the error interface type at hand.
// Used to check if types implement error.
var errorType reflect.Type = reflect.TypeOf(new(error)).Elem()

// kinds that are not supported for inputs or outputs of RPC functions.
var invalidKinds = map[reflect.Kind]struct{}{
	reflect.Chan:      struct{}{},
	reflect.Func:      struct{}{},
	reflect.Interface: struct{}{},
}

// A Caller mediates procedure calls between a Client and a Library, without having to hold information about either end.
type Caller interface {
	// The first argument is a slice of pointers to expected result values, excluding the final error.
	//
	// The second argument is a slice of argument values to be transmited to the external procedure.
	//
	// The third argument is a unique name for the procedure being called, agreed on by both processes.
	//
	// The result slice does not include the call error, which should be returned separately instead.
	// This is to allow usage of encodings that cannot directly handle interfaces, so the error can be treated internally as a string instead.
	Call([]reflect.Value, []reflect.Value, string) error
}

// Client represents an RPC Client.
type Client struct {
	c Caller
}

func MakeClient(c Caller) Client {
	return Client{c}
}

// Bind generates an RPC function through the Client, and stores it inside the given function pointer.
//
// Calls for this function will be made under the given name.
//
// fptr must be a non nil pointer to a function that returns an error as a final return value.
// All other return values and arguments must be concrete types.
func (x Client) Bind(name string, fptr any) error {
	fv := reflect.ValueOf(fptr).Elem()
	ft := fv.Type()

	if err := validateFunc(ft); err != nil {
		return err
	}

	numOut := ft.NumOut() - 1
	outTypes := make([]reflect.Type, numOut)
	for i := 0; i < numOut; i++ {
		outTypes[i] = ft.Out(i)
	}

	fn := func(args []reflect.Value) (results []reflect.Value) {
		// prepare pointers to return data types, except the error
		var err error
		results = make([]reflect.Value, numOut+1)
		for i := 0; i < numOut; i++ {
			results[i] = reflect.New(outTypes[i])
		}
		// dereference result pointers before returning
		defer func() {
			for i := 0; i < numOut; i++ {
				results[i] = results[i].Elem()
			}
			if err != nil {
				results[numOut] = reflect.ValueOf(err)
			} else {
				results[numOut] = reflect.Zero(errorType)
			}
		}()

		err = x.c.Call(results[:numOut], args, name)
		return
	}

	fv.Set(reflect.MakeFunc(ft, fn))
	return nil
}

// A Codec provides matching Encoders and Decoders, on demand.
// These can be used to sequantially encode or decode data.
type Codec interface {
	Decoder() Decoder
	Encoder() Encoder
}

// Decode calls should be capable of decoding data in the reverse order that it was encoded by a compatible Encoder,
// DecodeValues will always be called last, once.
type Decoder interface {
	Decode(any) error                   // decode value into arbitray pointer
	DecodeValues([]reflect.Value) error // decode values into a slice of arbitrary pointers
	Write([]byte) error                 // load data for sequential decoding
}

// A DualGate can handle both requests and responses on the same connection.
// If one side of the connection uses a DualGate, then both sides must do so.
type DualGate struct {
	codec  Codec
	conn   io.Writer // outgoing message destination
	dstNon io.Writer // incoming non rpc message destination

	disp dispatch  // outgoing request identifier
	res  Responder // incoming request responder
}

func NewDualGate(codec Codec, res Responder, conn io.ChainWriter, dstNon io.Writer) *DualGate {
	if dstNon == nil {
		dstNon = io.VoidWriter{}
	}
	x := &DualGate{
		codec:  codec,
		conn:   conn,
		dstNon: dstNon,
		disp:   *newDispatch(),
		res:    res,
	}
	conn.Chain(x)
	return x
}

func (x *DualGate) Request(enc Encoder) (Decoder, error) {
	id, ch := x.disp.provision()
	defer x.disp.release(id)

	// encode ID
	if err := enc.Encode(id); err != nil {
		return nil, err
	}

	// is an RPC request
	if err := enc.Encode(true); err != nil {
		return nil, err
	}

	// is an RPC message
	if err := enc.Encode(true); err != nil {
		return nil, err
	}

	// finalize encoding and send request
	b, err := enc.Read()
	if err != nil {
		return nil, err
	}
	if err = x.conn.Write(b); err != nil {
		return nil, err
	}

	// wait for response
	dec := <-ch
	return dec, nil
}

// Write accepts incoming messages.
func (x *DualGate) Write(b []byte) error {
	// don't return errors related to decoding a particular message

	dec := x.codec.Decoder()
	if err := dec.Write(b); err != nil {
		return nil
	}

	var isRpc bool
	if err := dec.Decode(&isRpc); err != nil {
		return nil
	}

	if !isRpc {
		var non []byte
		if err := dec.Decode(&non); err != nil {
			return nil
		}
		return x.dstNon.Write(non)
	}

	var isReq bool
	if err := dec.Decode(&isReq); err != nil {
		return nil
	}

	if !isReq {
		// decode request ID
		var id uint64
		if err := dec.Decode(&id); err != nil {
			return nil
		}

		x.disp.resolve(id, dec)
		return nil
	}

	enc, err := respondId(x.res, dec)
	if err != nil {
		if _, ok := err.(decodingError); ok {
			err = nil
		}
		return err
	}

	// is a response, not a request
	if err := enc.Encode(false); err != nil {
		return err
	}

	// is an RPC message
	if err := enc.Encode(true); err != nil {
		return err
	}

	if b, err = enc.Read(); err != nil {
		return err
	}
	return x.conn.Write(b)
}

// WriteNon writes a non-RPC message to the underlying connection.
func (x DualGate) WriteNon(b []byte) error {
	return writeNonTo(b, x.codec, x.conn)
}

// EncodeValues will always be called first, once.
type Encoder interface {
	Encode(any) error                   // encode arbitrary value
	EncodeValues([]reflect.Value) error // encode slice of arbitrary values
	Read() ([]byte, error)              // finalize, and return encoding
}

// An Interface helps maintain consistency between Go programs.
// This can be viewed as an extension of the concept of Go interface to RPC.
type Interface struct {
	Declare map[string]any // function pointers to bind to a client
	Define  map[string]any // actual functions to register with a library
}

// BindTo binds all of the Interface's declared methods to the specified client.
func (x Interface) BindTo(cli Client) error {
	for name, fptr := range x.Declare {
		if err := cli.Bind(name, fptr); err != nil {
			return err
		}
	}
	return nil
}

// RegisterWith makes all of the Interface's defined methods available through the specified Library.
// Returns an error if there are any declarations with no corresponding definition, or if their respective types don't match.
func (x Interface) RegisterWith(lib Library) error {
	for name, decl := range x.Declare {
		def := x.Define[name]
		if def == nil {
			return errors.New("missing definition")
		}

		declType := reflect.TypeOf(decl).Elem()
		defType := reflect.TypeOf(def)
		if declType != defType {
			return errors.New("definition doesn't match declaration")
		}

		if err := lib.Register(name, def); err != nil {
			return err
		}
	}
	return nil
}

// A Library registers procedures to be made available to external callers.
type Library struct {
	lib map[string]Procedure
}

func MakeLibrary() Library {
	return Library{
		lib: make(map[string]Procedure),
	}
}

func (x Library) AsResponder(c Codec) Responder {
	return libraryResponder{
		c: c,
		l: x,
	}
}

func (x Library) Get(name string) (Procedure, error) {
	p, ok := x.lib[name]
	if !ok {
		return Procedure{}, errors.New("unknown procedure " + name)
	}

	return p, nil
}

func (x Library) Register(name string, f any) error {
	p, err := makeProcedure(f)
	if err != nil {
		return err
	}

	x.lib[name] = p
	return nil
}

// A Procedure wraps functions to be usable by this package.
// Underlying function must have a final error output. All its other return values and arguments must be concrete types.
type Procedure struct {
	f      reflect.Value  // underlying function
	inType []reflect.Type // input types
}

// makeProcedure fails if f has non-concrete inputs or outputs, excluding a final error output.
func makeProcedure(f any) (Procedure, error) {
	t := reflect.TypeOf(f)

	if err := validateFunc(t); err != nil {
		return Procedure{}, err
	}

	if t.Kind() != reflect.Func {
		return Procedure{}, errors.New("not a function")
	}

	x := Procedure{
		f: reflect.ValueOf(f),
	}

	// store input types
	x.inType = make([]reflect.Type, t.NumIn())
	for i := 0; i < len(x.inType); i++ {
		x.inType[i] = t.In(i)
	}

	return x, nil
}

// args returns a set of pointers to the procedure's input types.
// These pointers must be dereferenced before using them in the "Call" method.
func (x Procedure) Args() []reflect.Value {
	o := make([]reflect.Value, len(x.inType))
	for i := 0; i < len(o); i++ {
		o[i] = reflect.New(x.inType[i])
	}
	return o
}

// call executes the underlying function with the provided input.
// Returns the final error separately from the other return values.
func (x Procedure) Call(in []reflect.Value) ([]reflect.Value, error) {
	r := x.f.Call(in)
	n := len(r) - 1
	if !r[n].IsNil() {
		return nil, r[n].Interface().(error)
	}

	return r[:n], nil
}

type Requester interface {
	Request(Encoder) (Decoder, error)
}

// A RequestGate wraps a basic connection that cannot automatically return the response to a specific request message by itself.
// Is concurrent safe if the connection is.
// Raw incoming messages, for example from a read loop, must be delivered to the RequestGate using its Write method.
type RequestGate struct {
	codec  Codec
	conn   io.Writer // outgoing message destination
	dstNon io.Writer // incoming non rpc message destination

	disp dispatch // request identification
}

// NewRequestGate returns a usable RequestGate.
// Writes raw rpc messages to conn.
// Will write incoming non-RPC messages to dstNon, which may be nil.
func NewRequestGate(codec Codec, conn io.ChainWriter, dstNon io.Writer) *RequestGate {
	if dstNon == nil {
		dstNon = io.VoidWriter{}
	}
	x := &RequestGate{
		codec:  codec,
		conn:   conn,
		dstNon: dstNon,
		disp:   *newDispatch(),
	}
	conn.Chain(x)
	return x
}

func (x *RequestGate) Request(enc Encoder) (Decoder, error) {
	// allocate call ID and associated response channel
	id, ch := x.disp.provision()

	// we let this method take responsibility for cleanup, as it might also be necessary on error
	defer x.disp.release(id)

	// encode ID
	if err := enc.Encode(id); err != nil {
		return nil, err
	}

	// is an RPC message
	if err := enc.Encode(true); err != nil {
		return nil, err
	}

	// finalize encoding and send request
	b, err := enc.Read()
	if err != nil {
		return nil, err
	}
	if err := x.conn.Write(b); err != nil {
		return nil, err
	}

	// wait for response
	dec := <-ch
	return dec, nil
}

// Write accepts incoming raw messages from a connection.
func (x *RequestGate) Write(b []byte) error {
	// don't return errors related to decoding a particular message

	dec := x.codec.Decoder()
	if err := dec.Write(b); err != nil {
		return nil
	}

	var isRpc bool
	if err := dec.Decode(&isRpc); err != nil {
		return nil
	}

	if !isRpc {
		var non []byte
		if err := dec.Decode(&non); err != nil {
			return nil
		}
		return x.dstNon.Write(non)
	}

	// decode call ID
	var id uint64
	if err := dec.Decode(&id); err != nil {
		return nil
	}

	x.disp.resolve(id, dec)
	return nil
}

// WriteNon writes a non-RPC message to the underlying connection.
// To be used instead of writing to it directly.
func (x RequestGate) WriteNon(b []byte) error {
	return writeNonTo(b, x.codec, x.conn)
}

type Responder interface {
	Respond(Decoder) (Encoder, error)
}

// A ResponseGate is the counterpart to a RequestGate.
type ResponseGate struct {
	codec  Codec
	res    Responder
	conn   io.Writer // outgoing message destination
	dstNon io.Writer // incoming non-RPC message destination
}

// MakeResponseGate returns a usable ResponseGate that writes incoming non-RPC messages to dstNon, which may be nil.
// Sends raw RPC responses to conn.
func MakeResponseGate(codec Codec, res Responder, conn io.ChainWriter, dstNon io.Writer) ResponseGate {
	if dstNon == nil {
		dstNon = io.VoidWriter{}
	}
	x := ResponseGate{
		codec:  codec,
		res:    res,
		conn:   conn,
		dstNon: dstNon,
	}
	conn.Chain(x)
	return x
}

// Write accepts incoming raw messages from a connection.
func (x ResponseGate) Write(b []byte) error {
	// don't return errors related to decoding a particular message

	dec := x.codec.Decoder()
	if err := dec.Write(b); err != nil {
		return nil
	}

	var isRpc bool
	if err := dec.Decode(&isRpc); err != nil {
		return nil
	}

	if !isRpc {
		var non []byte
		if err := dec.Decode(&non); err != nil {
			return nil
		}
		return x.dstNon.Write(non)
	}

	enc, err := respondId(x.res, dec)
	if err != nil {
		if _, ok := err.(decodingError); ok {
			err = nil
		}
		return err
	}

	if err := enc.Encode(true); err != nil {
		return err
	}
	if b, err = enc.Read(); err != nil {
		return err
	}

	return x.conn.Write(b)
}

// WriteNon writes a non-rpc message to the underlying connection.
// To be used instead of writing directly.
func (x ResponseGate) WriteNon(b []byte) error {
	return writeNonTo(b, x.codec, x.conn)
}

type caller struct {
	c Codec
	r Requester
}

// MakeCaller bundles a Codec and a Processor together to function as a Caller.
func MakeCaller(c Codec, r Requester) Caller {
	return caller{
		c: c,
		r: r,
	}
}

func (x caller) Call(res, args []reflect.Value, name string) error {
	// encode call name + arguments
	enc := x.c.Encoder()
	if err := enc.EncodeValues(args); err != nil {
		return err
	}
	if err := enc.Encode(name); err != nil {
		return err
	}

	// send call and get response
	dec, err := x.r.Request(enc)
	if err != nil {
		return err
	}

	// check error
	var errStr string
	if err := dec.Decode(&errStr); err != nil {
		return err
	}
	if errStr != "" {
		return errors.New(errStr)
	}

	// decode results
	return dec.DecodeValues(res)
}

// decodingError wraps decoding errors.
// Read loops should generally not crash because of malformed messages.
// This type allows distinguising between the various errors that can arise in a Respond chain.
type decodingError struct {
	err error // wrapped error
}

func (x decodingError) Error() string {
	return "decoding error"
}

func (x decodingError) Unwrap() error {
	return x.err
}

type dispatch struct {
	next    uint64
	pending map[uint64]chan Decoder
	mux     sync.Mutex
}

func newDispatch() *dispatch {
	return &dispatch{
		pending: make(map[uint64]chan Decoder),
	}
}

// provision returns a unique ID and a channel to receive an asynchronous response.
// It is the caller's responsibility to release the provided ID.
func (x *dispatch) provision() (uint64, chan Decoder) {
	ch := make(chan Decoder, 1) // cap of 1 so the resolver doesn't block if the provisioning side goroutine exits prematurely

	x.mux.Lock()

	// normally there should be a check for pending IDs, but that should really never be necessary
	id := x.next
	x.pending[id] = ch

	x.mux.Unlock()

	return id, ch
}

// release cleans up the specified ID.
func (x *dispatch) release(id uint64) {
	x.mux.Lock()
	delete(x.pending, id)
	x.mux.Unlock()
}

// resolve delivers a response to the appropriate channel.
func (x *dispatch) resolve(id uint64, resp Decoder) {
	x.mux.Lock()
	ch, ok := x.pending[id]
	x.mux.Unlock()

	if ok {
		ch <- resp
	}
}

type libraryResponder struct {
	c Codec
	l Library
}

func (x libraryResponder) Respond(dec Decoder) (Encoder, error) {
	// decode procedure name
	var name string
	if err := dec.Decode(&name); err != nil {
		return nil, decodingError{err}
	}

	// get procedure
	proc, err := x.l.Get(name)
	if err != nil {
		return nil, err
	}

	// get argument pointers
	vals := proc.Args()
	if err = dec.DecodeValues(vals); err != nil {
		return nil, decodingError{err}
	}

	// dereference pointers
	for i := range vals {
		vals[i] = vals[i].Elem()
	}

	// make function call
	vals, err = proc.Call(vals)

	// call error is encoded back as a string
	var errStr string
	if err != nil {
		errStr = err.Error()
	}

	enc := x.c.Encoder()
	if err = enc.EncodeValues(vals); err != nil {
		return nil, err
	}
	if err = enc.Encode(errStr); err != nil {
		return nil, err
	}

	return enc, nil
}

type processor struct {
	c Codec
	r Responder
}

// MakeProcessor bundles a Codec and a Responder to function as an io.Processor.
// Serves as basic server side intermediary between raw binary and RPC logic.
func MakeProcessor(c Codec, r Responder) io.Processor {
	return processor{
		c: c,
		r: r,
	}
}

func (x processor) Process(b []byte) ([]byte, error) {
	dec := x.c.Decoder()
	if err := dec.Write(b); err != nil {
		return nil, err
	}

	enc, err := x.r.Respond(dec)
	if err != nil {
		return nil, err
	}

	return enc.Read()
}

type requester struct {
	c Codec
	p io.Processor
}

// MakeRequester bundles a Codec and an io.Processor to function as a basic Requester.
// Suitable for use when the underlying connection is capable of directly returning the response to a specific request message, by itself.
func MakeRequester(c Codec, p io.Processor) Requester {
	return requester{
		c: c,
		p: p,
	}
}

func (x requester) Request(enc Encoder) (Decoder, error) {
	// get binary encoding
	b, err := enc.Read()
	if err != nil {
		return nil, err
	}

	// send request and wait for response
	if b, err = x.p.Process(b); err != nil {
		return nil, err
	}

	// wrap and return response
	dec := x.c.Decoder()
	if err = dec.Write(b); err != nil {
		return nil, err
	}

	return dec, nil
}

// respondId decodes a request ID and encodes it back into a response.
func respondId(res Responder, dec Decoder) (Encoder, error) {
	// decode call ID, encode it back unchanged when responding
	var id uint64
	if err := dec.Decode(&id); err != nil {
		// return early if we can't even identify the call
		return nil, decodingError{err}
	}

	enc, err := res.Respond(dec)
	if err != nil {
		return nil, err
	}
	if err := enc.Encode(id); err != nil {
		return nil, err
	}

	return enc, nil
}

// validateFunc checks the given function type.
// Returns an error if it isn't supported by this package.
//TODO deep check of input and output types.
func validateFunc(ft reflect.Type) error {
	if ft.Kind() != reflect.Func {
		return errors.New("not a function")
	}

	// check inputs
	for i, n := 0, ft.NumIn(); i < n; i++ {
		k := ft.In(i).Kind()
		if _, ok := invalidKinds[k]; ok {
			return errors.New("unsupported input kind " + k.String())
		}
	}

	// check error
	numOut := ft.NumOut() - 1
	if numOut < 0 || ft.Out(numOut) != errorType {
		return errors.New("function does not return an error")
	}

	// check rest of outputs
	for i := 0; i < numOut; i++ {
		outType := ft.Out(i)
		k := outType.Kind()
		if _, ok := invalidKinds[k]; ok {
			return errors.New("unsupported output kind " + k.String())
		}
	}

	return nil
}

// writeNonTo encodes and writes a non-rpc message
func writeNonTo(b []byte, c Codec, w io.Writer) error {
	enc := c.Encoder()
	if err := enc.Encode(b); err != nil {
		return err
	}
	if err := enc.Encode(false); err != nil {
		return err
	}
	r, err := enc.Read()
	if err != nil {
		return err
	}
	return w.Write(r)
}
