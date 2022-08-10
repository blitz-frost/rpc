/*
Package rpc provides bridging of function calls between two Go programs.

The main types are [Client] on the call side, and [Library] on the answer side. [Caller] is left as an interface that importers may implement in order to use custom call mechanisms.
Otherwise, [CallGate] and [AnswerGate] are the two reference types used to bridge between a Client and a Library that reside on different processes/machines.

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

The answer side would then look something like this:

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

The caller side:

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

	"github.com/blitz-frost/conv"
	"github.com/blitz-frost/encoding"
)

type AnswerGate struct {
	lib Library
}

func MakeAnswerGate(lib Library) AnswerGate {
	return AnswerGate{lib}
}

func (x AnswerGate) DecodeFrom(dec encoding.ExchangeDecoder) error {
	// don't return decode errors or missing procedure; silently drop
	nameVal, err := dec.Decode(typeString)
	if err != nil {
		dec.Close()
		return nil
	}

	proc, err := x.lib.Get(nameVal.String())
	if err != nil {
		dec.Close()
		return nil
	}

	argTypes := proc.Args()
	argValues := make([]reflect.Value, len(argTypes))
	for i, t := range argTypes {
		if argValues[i], err = dec.Decode(t); err != nil {
			dec.Close()
			return nil
		}
	}
	dec.Close()

	results, errCall := proc.Call(argValues)

	enc, err := dec.Encoder()
	if err != nil {
		return err
	}

	var errStr string
	if errCall != nil {
		errStr = errCall.Error()
	}

	if err := enc.Encode(reflect.ValueOf(errStr)); err != nil {
		return enc.Close()
	}

	// don't bother encoding the output if we have an error
	if errCall != nil {
		return enc.Close()
	}

	for _, v := range results {
		if err := enc.Encode(v); err != nil {
			return enc.Close()
		}
	}

	return enc.Close()
}

type CallGate struct {
	eeg encoding.ExchangeEncoderGiver
}

func MakeCallGate(eeg encoding.ExchangeEncoderGiver) CallGate {
	return CallGate{eeg}
}

func (x CallGate) Call(name string, args []reflect.Value, outTypes []reflect.Type) (result []reflect.Value, err error) {
	nOut := len(outTypes)
	result = make([]reflect.Value, nOut, nOut+1) // provide capacity for error value appending
	defer func() {
		if err != nil {
			for i := range result {
				result[i] = reflect.Zero(outTypes[i])
			}
		}
	}()

	enc, err := x.eeg.Encoder()
	if err != nil {
		return
	}

	if err = enc.Encode(reflect.ValueOf(name)); err != nil {
		enc.Close()
		return
	}
	for i := range args {
		if err = enc.Encode(args[i]); err != nil {
			enc.Close()
			return
		}
	}

	// send call and get response
	dec, err := enc.Decoder()
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
//
// Importers don't generally need to use this type directly, but it is exported for custom call mechanism implementations.
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

var (
	typeError  = reflect.TypeOf(new(error)).Elem()
	typeString = reflect.TypeOf("")
)

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
