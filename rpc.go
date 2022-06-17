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

	"github.com/blitz-frost/conv"
	"github.com/blitz-frost/io/msg"
)

// A Caller mediates procedure calls between a Client and a Library, without having to hold information about either end.
type Caller interface {
	// The first argument is a unique name for the procedure being called, agreed on by both processes.
	//
	// The second argument is a slice of argument values to be transmited to the external procedure.
	//
	// The third argument is a slice of types of the expected return values, excluding the final error.
	//
	// The result slice does not include the call error, which should be returned separately instead.
	// This is to allow usage of encodings that cannot directly handle interfaces, so the error can be treated internally as a string instead.
	// The slice must contain valid values of the appropriate type.
	Call(string, []reflect.Value, []reflect.Type) ([]reflect.Value, error)
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
		o, err := x.c.Call(name, args, outTypes)
		oErr := reflect.Zero(typeError)
		if err != nil {
			oErr = reflect.ValueOf(err)
		}
		return append(o, oErr)
	}

	fv.Set(reflect.MakeFunc(ft, fn))
	return nil
}

// BindClass binds all the functions in a class pointer.
// See Library.RegisterClass for an explanation on classes.
func (x Client) BindClass(classPtr any) error {
	v := reflect.ValueOf(classPtr)
	if v.Kind() != reflect.Pointer {
		return errors.New("not a pointer")
	}

	v = v.Elem()
	t := v.Type()
	if t.Kind() != reflect.Struct {
		return errors.New("not a class pointer")
	}

	for i, n := 0, t.NumField(); i < n; i++ {
		field := t.Field(i)
		if !field.IsExported() {
			return errors.New("unexported field")
		}

		if field.Type.Kind() != reflect.Func {
			return errors.New("non-function field")
		}

		fptr := v.Field(i).Addr().Interface()
		if err := x.Bind(field.Name, fptr); err != nil {
			return err
		}
	}

	return nil
}

// A Codec provides matching Encoders and Decoders, on demand.
// These can be used to sequantially encode or decode data.
type Codec interface {
	Decoder(msg.Reader) Decoder
	Encoder(msg.Writer) Encoder
}

type CodecConn interface {
	Encoder() (Encoder, error)
	ChainDecode(DecoderFrom) error
}

type Conn interface {
	msg.WriterSource
	msg.ReadChainer
}

type Decoder interface {
	Reader() (msg.Reader, error) // expose raw byte reader, at current position; Decoder should no longer be used
	Decode(reflect.Type) (reflect.Value, error)
	Close() error
}

type DecoderFrom interface {
	DecodeFrom(Decoder) error
}

// A DualGate filters between RPC requests and responses.
// If one side of the connection uses a DualGate, then both sides must do so.
type DualGate struct {
	conn CodecConn

	dstReq DecoderFrom
	dstRes DecoderFrom
}

func NewDualGate(conn CodecConn) *DualGate {
	x := &DualGate{
		conn: conn,
	}
	conn.ChainDecode(x)
	return x
}

// AsConnResp returns the DualGate as a CodecConn for use by ResponseGates.
func (x *DualGate) AsConnResp() CodecConn {
	return dualGate{x}
}

func (x *DualGate) ChainDecode(dstReq DecoderFrom) error {
	x.dstReq = dstReq
	return nil
}

func (x *DualGate) ChainDecodeResp(dstRes DecoderFrom) error {
	x.dstRes = dstRes
	return nil
}

func (x *DualGate) DecodeFrom(dec Decoder) error {
	isReqVal, err := dec.Decode(typeBool)
	if err != nil {
		dec.Close()
		return nil
	}

	if isReqVal.Bool() {
		return x.dstReq.DecodeFrom(dec)
	}

	return x.dstRes.DecodeFrom(dec)
}

func (x *DualGate) Encoder() (Encoder, error) {
	enc, err := x.conn.Encoder()
	if err != nil {
		return nil, err
	}

	// is request
	if err = enc.Encode(reflect.ValueOf(true)); err != nil {
		enc.Cancel()
		return nil, err
	}

	return enc, nil
}

func (x *DualGate) EncoderResp() (Encoder, error) {
	enc, err := x.conn.Encoder()
	if err != nil {
		return nil, err
	}

	// is not request
	if err = enc.Encode(reflect.ValueOf(false)); err != nil {
		enc.Cancel()
		return nil, err
	}

	return enc, nil
}

type Encoder interface {
	Writer() (msg.Writer, error) // expose raw byte writer; Encoder should finalize and it should not be used anymore
	Encode(reflect.Value) error
	Close() error
	Cancel()
}

// A Gate filters RPC from non-RPC messages.
// To be used when the underlying connection is not used exclusively for RPC data transfer.
// If one side uses a Gate, them both sides must do so.
//
// Users should use the Gate.Writer method to write to the underlying connection.
type Gate struct {
	conn CodecConn

	dstRpc DecoderFrom
	dstNon msg.ReaderFrom
}

// Wraps conn as a Gate. Both RPC and non-RPC destinations must be set before usage.
func NewGate(conn CodecConn) *Gate {
	x := &Gate{
		conn: conn,
	}
	conn.ChainDecode(x)
	return x
}

// set RPC message destination
func (x *Gate) ChainDecode(dst DecoderFrom) error {
	x.dstRpc = dst
	return nil
}

// set non-RPC message destination
func (x *Gate) ChainRead(dst msg.ReaderFrom) error {
	x.dstNon = dst
	return nil
}

func (x *Gate) DecodeFrom(dec Decoder) error {
	// is RPC?
	isRpcVal, err := dec.Decode(typeBool)
	if err != nil {
		// silently drop unexpected messages
		dec.Close()
		return nil
	}

	if isRpcVal.Bool() {
		return x.dstRpc.DecodeFrom(dec)
	}

	r, err := dec.Reader()
	if err != nil {
		dec.Close()
		return err
	}
	return x.dstNon.ReadFrom(r)
}

func (x *Gate) Encoder() (Encoder, error) {
	enc, err := x.conn.Encoder()
	if err != nil {
		return nil, err
	}

	// is an RPC message
	if err = enc.Encode(reflect.ValueOf(true)); err != nil {
		return nil, err
	}

	return enc, err
}

// Writer returns a writer for non-RPC data. To be used instead of requesting one from the underling connection directly.
func (x *Gate) Writer() (msg.Writer, error) {
	enc, err := x.conn.Encoder()
	if err != nil {
		return nil, err
	}

	// is not an RPC message
	if err = enc.Encode(reflect.ValueOf(false)); err != nil {
		return nil, err
	}

	return enc.Writer()
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

/* RegisterClass registers all the functions of a class, using their member names.

A class is an informal struct type containing only exported function members. Example:

	type SomeClass struct {
		SomeFunc func()
		OtherFunc func(int) error
	}

The primary purpose of classes is to define method set contracts in cross packages.
The Interface type should be more convenient for global function contracts, but the two approaches are essentially interchangable.
*/
func (x Library) RegisterClass(class any) error {
	v := reflect.ValueOf(class)
	t := v.Type()

	if t.Kind() != reflect.Struct {
		return errors.New("not a class")
	}

	for i, n := 0, t.NumField(); i < n; i++ {
		field := t.Field(i)
		if !field.IsExported() {
			return errors.New("unexported field")
		}

		if field.Type.Kind() != reflect.Func {
			return errors.New("non-function field")
		}

		fn := v.Field(i).Interface()
		if err := x.Register(field.Name, fn); err != nil {
			return err
		}
	}

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

// Args returns the procedure's input types.
func (x Procedure) Args() []reflect.Type {
	return x.inType
}

// Call executes the underlying function with the provided input.
// Returns the final error separately from the other return values.
func (x Procedure) Call(in []reflect.Value) ([]reflect.Value, error) {
	r := x.f.Call(in)
	n := len(r) - 1
	if !r[n].IsNil() {
		return nil, r[n].Interface().(error)
	}

	return r[:n], nil
}

type Request interface {
	Encode(reflect.Value) error
	Do() (Decoder, error)
	Cancel()
}

type RequestGate struct {
	conn CodecConn

	disp dispatch // request identification
}

func NewRequestGate(conn CodecConn) *RequestGate {
	x := &RequestGate{
		conn: conn,
		disp: *newDispatch(),
	}
	conn.ChainDecode(x)
	return x
}

// DecodeFrom accepts incoming encoded messages from a connection.
func (x *RequestGate) DecodeFrom(dec Decoder) error {
	// don't return errors related to decoding a particular message

	// decode call ID
	idVal, err := dec.Decode(typeUint64)
	if err != nil {
		dec.Close()
		return nil
	}

	x.disp.resolve(idVal.Uint(), response{
		dec: dec,
		err: nil,
	})
	return nil
}

func (x *RequestGate) Request() (Request, error) {
	// allocate call ID and associated response channel
	// do this before potentially blocking the connection for encoding
	id, ch := x.disp.provision()

	enc, err := x.conn.Encoder()
	if err != nil {
		x.disp.release(id)
		return nil, err
	}

	req := request{
		id:     id,
		cancel: x.disp.release,
		enc:    enc,
		ch:     ch,
	}

	// encode ID
	if err := enc.Encode(reflect.ValueOf(id)); err != nil {
		req.Cancel()
		return nil, err
	}

	return req, nil
}

type Requester interface {
	Request() (Request, error)
}

type ResponseGate struct {
	conn CodecConn
	lib  Library
}

func MakeResponseGate(conn CodecConn, lib Library) ResponseGate {
	x := ResponseGate{
		conn: conn,
		lib:  lib,
	}
	conn.ChainDecode(x)
	return x
}

func (x ResponseGate) DecodeFrom(dec Decoder) (err error) {
	idVal, errDec := dec.Decode(typeUint64)
	defer func() {
		if errDec != nil {
			dec.Close()
		}
	}()
	if errDec != nil {
		return nil
	}

	nameVal, errDec := dec.Decode(typeString)
	if errDec != nil {
		return nil
	}

	var (
		results []reflect.Value
		errCall error
	)
	defer func() {
		// send no response if input could not be decoded
		if errDec != nil {
			return
		}

		var enc Encoder
		enc, err = x.conn.Encoder()
		if err != nil {
			return
		}

		defer func() {
			if err != nil {
				enc.Cancel()
			} else {
				enc.Close()
			}
		}()

		if err = enc.Encode(idVal); err != nil {
			return
		}

		var errStr string
		if errCall != nil {
			errStr = errCall.Error()
		}

		if err = enc.Encode(reflect.ValueOf(errStr)); err != nil {
			return
		}

		// don't bother encoding the output if we have an error
		if errCall != nil {
			return
		}

		for _, v := range results {
			if err = enc.Encode(v); err != nil {
				return
			}
		}
	}()

	proc, errCall := x.lib.Get(nameVal.String())
	if errCall != nil {
		dec.Close()
		return nil
	}

	argTypes := proc.Args()
	argValues := make([]reflect.Value, len(argTypes))
	for i, t := range argTypes {
		if argValues[i], errDec = dec.Decode(t); err != nil {
			return nil
		}
	}
	dec.Close()

	results, errCall = proc.Call(argValues)

	return
}

var (
	typeBool   = reflect.TypeOf(true)
	typeError  = reflect.TypeOf(new(error)).Elem()
	typeString = reflect.TypeOf("")
	typeUint64 = reflect.TypeOf(uint64(0))
)

type caller struct {
	r Requester
}

// MakeCaller wraps a Requester to function as a Caller.
func MakeCaller(r Requester) Caller {
	return caller{
		r: r,
	}
}

func (x caller) Call(name string, args []reflect.Value, outTypes []reflect.Type) (result []reflect.Value, err error) {
	nOut := len(outTypes)
	result = make([]reflect.Value, nOut, nOut+1) // provide capacity for error value appending
	defer func() {
		if err != nil {
			for i := range result {
				result[i] = reflect.Zero(outTypes[i])
			}
		}
	}()

	req, err := x.r.Request()
	if err != nil {
		return
	}

	if err = req.Encode(reflect.ValueOf(name)); err != nil {
		req.Cancel()
		return
	}
	for i := range args {
		if err = req.Encode(args[i]); err != nil {
			req.Cancel()
			return
		}
	}

	// send call and get response
	dec, err := req.Do()
	if err != nil {
		return
	}
	defer dec.Close()

	// check error
	errVal, err := dec.Decode(typeString)
	if err != nil {
		return
	}
	if errStr := errVal.String(); errStr != "" {
		err = errors.New(errStr)
		return
	}

	// decode results
	for i := range result {
		if result[i], err = dec.Decode(outTypes[i]); err != nil {
			return
		}
	}

	return
}

type codecConn struct {
	conn  Conn
	codec Codec

	dst DecoderFrom
}

func WrapConn(conn Conn, codec Codec) CodecConn {
	x := &codecConn{
		conn:  conn,
		codec: codec,
	}
	conn.ChainRead(x)
	return x
}

func (x *codecConn) ChainDecode(dst DecoderFrom) error {
	x.dst = dst
	return nil
}

func (x codecConn) Encoder() (Encoder, error) {
	w, err := x.conn.Writer()
	if err != nil {
		return nil, err
	}
	return x.codec.Encoder(w), nil
}

func (x codecConn) ReadFrom(r msg.Reader) error {
	dec := x.codec.Decoder(r)
	return x.dst.DecodeFrom(dec)
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
	pending map[uint64]chan response
	mux     sync.Mutex
}

func newDispatch() *dispatch {
	return &dispatch{
		pending: make(map[uint64]chan response),
	}
}

// provision returns a unique ID and a channel to receive an asynchronous response.
// It is the caller's responsibility to resolve or release the provided ID.
func (x *dispatch) provision() (uint64, chan response) {
	ch := make(chan response, 1) // cap of 1 so the resolver doesn't block if the provisioning side goroutine exits prematurely

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
func (x *dispatch) resolve(id uint64, resp response) {
	x.mux.Lock()
	ch, ok := x.pending[id]
	delete(x.pending, id)
	x.mux.Unlock()

	if ok {
		ch <- resp
	}
}

// dualGate is the response variant of DualGate
type dualGate struct {
	v *DualGate
}

func (x dualGate) ChainDecode(dst DecoderFrom) error {
	return x.v.ChainDecodeResp(dst)
}

func (x dualGate) Encoder() (Encoder, error) {
	return x.v.EncoderResp()
}

// used with dispatch type
type request struct {
	id     uint64
	cancel func(uint64)

	enc Encoder
	ch  chan response
}

func (x request) Cancel() {
	x.enc.Cancel()
	x.cancel(x.id)
}

func (x request) Do() (Decoder, error) {
	resp := <-x.ch
	return resp.dec, resp.err
}

func (x request) Encode(v reflect.Value) error {
	return x.enc.Encode(v)
}

// used to deliver Request response through channel
type response struct {
	dec Decoder
	err error
}

// check is used to recursively check input and output types.
func check(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Chan, reflect.Interface, reflect.Func:
		return false
	}
	return true
}

// validateFunc checks the given function type.
// Returns an error if it isn't supported by this package.
func validateFunc(ft reflect.Type) error {
	if ft.Kind() != reflect.Func {
		return errors.New("not a function")
	}

	// check inputs
	for i, n := 0, ft.NumIn(); i < n; i++ {
		if t := ft.In(i); !conv.Check(t, check) {
			return errors.New("unsupported input type " + t.String())
		}
	}

	// check error
	numOut := ft.NumOut() - 1
	if numOut < 0 || ft.Out(numOut) != typeError {
		return errors.New("function does not return an error")
	}

	// check rest of outputs
	for i := 0; i < numOut; i++ {
		if t := ft.Out(i); !conv.Check(t, check) {
			return errors.New("unsupported output type " + t.String())
		}
	}

	return nil
}
