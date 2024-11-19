// Copyright 2022 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package js

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"

	"github.com/DoModond/go-ethereum/core/tracing"
	"github.com/DoModond/go-ethereum/core/types"
	"github.com/DoModond/go-ethereum/eth/tracers"
	"github.com/DoModond/go-ethereum/eth/tracers/internal"
	"github.com/DoModond/go-ethereum/params"
	"github.com/dop251/goja"
	"github.com/holiman/uint256"

	"github.com/DoModond/go-ethereum/common"
	"github.com/DoModond/go-ethereum/common/hexutil"
	"github.com/DoModond/go-ethereum/core/vm"
	"github.com/DoModond/go-ethereum/crypto"
	jsassets "github.com/DoModond/go-ethereum/eth/tracers/js/internal/tracers"
)

var assetTracers = make(map[string]string)

// init retrieves the JavaScript transaction tracers included in go-ethereum.
func init() {
	var err error
	assetTracers, err = jsassets.Load()
	if err != nil {
		panic(err)
	}
	type ctorFn = func(*tracers.Context, json.RawMessage, *params.ChainConfig) (*tracers.Tracer, error)
	lookup := func(code string) ctorFn {
		return func(ctx *tracers.Context, cfg json.RawMessage, chainConfig *params.ChainConfig) (*tracers.Tracer, error) {
			return newJsTracer(code, ctx, cfg, chainConfig)
		}
	}
	for name, code := range assetTracers {
		tracers.DefaultDirectory.Register(name, lookup(code), true)
	}
	tracers.DefaultDirectory.RegisterJSEval(newJsTracer)
}

var compiledBigInt *goja.Program
var compileOnce sync.Once

// getBigIntProgram compiles the bigint library, if needed, and returns the compiled
// goja program.
func getBigIntProgram() *goja.Program {
	compileOnce.Do(func() {
		compiledBigInt = goja.MustCompile("bigInt", bigIntegerJS, false)
	})
	return compiledBigInt
}

type toBigFn = func(vm *goja.Runtime, val string) (goja.Value, error)
type toBufFn = func(vm *goja.Runtime, val []byte) (goja.Value, error)
type fromBufFn = func(vm *goja.Runtime, buf goja.Value, allowString bool) ([]byte, error)

func toBuf(vm *goja.Runtime, bufType goja.Value, val []byte) (goja.Value, error) {
	// bufType is usually Uint8Array. This is equivalent to `new Uint8Array(val)` in JS.
	return vm.New(bufType, vm.ToValue(vm.NewArrayBuffer(val)))
}

func fromBuf(vm *goja.Runtime, bufType goja.Value, buf goja.Value, allowString bool) ([]byte, error) {
	obj := buf.ToObject(vm)
	switch obj.ClassName() {
	case "String":
		if !allowString {
			break
		}
		return common.FromHex(obj.String()), nil

	case "Array":
		var b []byte
		if err := vm.ExportTo(buf, &b); err != nil {
			return nil, err
		}
		return b, nil

	case "Object":
		if !obj.Get("constructor").SameAs(bufType) {
			break
		}
		b := obj.Export().([]byte)
		return b, nil
	}
	return nil, errors.New("invalid buffer type")
}

// jsTracer is an implementation of the Tracer interface which evaluates
// JS functions on the relevant EVM hooks. It uses Goja as its JS engine.
type jsTracer struct {
	vm                *goja.Runtime
	env               *tracing.VMContext
	chainConfig       *params.ChainConfig
	toBig             toBigFn               // Converts a hex string into a JS bigint
	toBuf             toBufFn               // Converts a []byte into a JS buffer
	fromBuf           fromBufFn             // Converts an array, hex string or Uint8Array to a []byte
	ctx               map[string]goja.Value // KV-bag passed to JS in `result`
	activePrecompiles []common.Address      // List of active precompiles at current block
	traceStep         bool                  // True if tracer object exposes a `step()` method
	traceFrame        bool                  // True if tracer object exposes the `enter()` and `exit()` methods
	err               error                 // Any error that should stop tracing
	obj               *goja.Object          // Trace object

	// Methods exposed by tracer
	result goja.Callable
	fault  goja.Callable
	step   goja.Callable
	enter  goja.Callable
	exit   goja.Callable

	// Underlying structs being passed into JS
	log         *steplog
	frame       *callframe
	frameResult *callframeResult

	// Goja-wrapping of types prepared for JS consumption
	logValue         goja.Value
	dbValue          goja.Value
	frameValue       goja.Value
	frameResultValue goja.Value
}

// newJsTracer instantiates a new JS tracer instance. code is a
// Javascript snippet which evaluates to an expression returning
// an object with certain methods:
//
// The methods `result` and `fault` are required to be present.
// The methods `step`, `enter`, and `exit` are optional, but note that
// `enter` and `exit` always go together.
func newJsTracer(code string, ctx *tracers.Context, cfg json.RawMessage, chainConfig *params.ChainConfig) (*tracers.Tracer, error) {
	vm := goja.New()
	// By default field names are exported to JS as is, i.e. capitalized.
	vm.SetFieldNameMapper(goja.UncapFieldNameMapper())
	t := &jsTracer{
		vm:          vm,
		ctx:         make(map[string]goja.Value),
		chainConfig: chainConfig,
	}

	t.setTypeConverters()
	t.setBuiltinFunctions()

	if ctx == nil {
		ctx = new(tracers.Context)
	}
	if ctx.BlockHash != (common.Hash{}) {
		blockHash, err := t.toBuf(vm, ctx.BlockHash.Bytes())
		if err != nil {
			return nil, err
		}
		t.ctx["blockHash"] = blockHash
		if ctx.TxHash != (common.Hash{}) {
			t.ctx["txIndex"] = vm.ToValue(ctx.TxIndex)
			txHash, err := t.toBuf(vm, ctx.TxHash.Bytes())
			if err != nil {
				return nil, err
			}
			t.ctx["txHash"] = txHash
		}
	}

	ret, err := vm.RunString("(" + code + ")")
	if err != nil {
		return nil, err
	}
	// Check tracer's interface for required and optional methods.
	obj := ret.ToObject(vm)
	result, ok := goja.AssertFunction(obj.Get("result"))
	if !ok {
		return nil, errors.New("trace object must expose a function result()")
	}
	fault, ok := goja.AssertFunction(obj.Get("fault"))
	if !ok {
		return nil, errors.New("trace object must expose a function fault()")
	}
	step, ok := goja.AssertFunction(obj.Get("step"))
	t.traceStep = ok
	enter, hasEnter := goja.AssertFunction(obj.Get("enter"))
	exit, hasExit := goja.AssertFunction(obj.Get("exit"))
	if hasEnter != hasExit {
		return nil, errors.New("trace object must expose either both or none of enter() and exit()")
	}
	t.traceFrame = hasEnter
	t.obj = obj
	t.step = step
	t.enter = enter
	t.exit = exit
	t.result = result
	t.fault = fault

	// Pass in config
	if setup, ok := goja.AssertFunction(obj.Get("setup")); ok {
		cfgStr := "{}"
		if cfg != nil {
			cfgStr = string(cfg)
		}
		if _, err := setup(obj, vm.ToValue(cfgStr)); err != nil {
			return nil, err
		}
	}
	// Setup objects carrying data to JS. These are created once and re-used.
	t.log = &steplog{
		vm:       vm,
		op:       &opObj{vm: vm},
		memory:   &memoryObj{vm: vm, toBig: t.toBig, toBuf: t.toBuf},
		stack:    &stackObj{vm: vm, toBig: t.toBig},
		contract: &contractObj{vm: vm, toBig: t.toBig, toBuf: t.toBuf},
	}
	t.frame = &callframe{vm: vm, toBig: t.toBig, toBuf: t.toBuf}
	t.frameResult = &callframeResult{vm: vm, toBuf: t.toBuf}
	t.frameValue = t.frame.setupObject()
	t.frameResultValue = t.frameResult.setupObject()
	t.logValue = t.log.setupObject()

	return &tracers.Tracer{
		Hooks: &tracing.Hooks{
			OnTxStart: t.OnTxStart,
			OnTxEnd:   t.OnTxEnd,
			OnEnter:   t.OnEnter,
			OnExit:    t.OnExit,
			OnOpcode:  t.OnOpcode,
			OnFault:   t.OnFault,
		},
		GetResult: t.GetResult,
		Stop:      t.Stop,
	}, nil
}

// OnTxStart implements the Tracer interface and is invoked at the beginning of
// transaction processing.
func (t *jsTracer) OnTxStart(env *tracing.VMContext, tx *types.Transaction, from common.Address) {
	t.env = env
	// Need statedb access for db object
	db := &dbObj{db: env.StateDB, vm: t.vm, toBig: t.toBig, toBuf: t.toBuf, fromBuf: t.fromBuf}
	t.dbValue = db.setupObject()
	// Update list of precompiles based on current block
	rules := t.chainConfig.Rules(env.BlockNumber, env.Random != nil, env.Time)
	t.activePrecompiles = vm.ActivePrecompiles(rules)
	t.ctx["block"] = t.vm.ToValue(t.env.BlockNumber.Uint64())
	t.ctx["gas"] = t.vm.ToValue(tx.Gas())
	gasPriceBig, err := t.toBig(t.vm, env.GasPrice.String())
	if err != nil {
		t.err = err
		return
	}
	t.ctx["gasPrice"] = gasPriceBig
	coinbase, err := t.toBuf(t.vm, env.Coinbase.Bytes())
	if err != nil {
		t.err = err
		return
	}
	t.ctx["coinbase"] = t.vm.ToValue(coinbase)
}

// OnTxEnd implements the Tracer interface and is invoked at the end of
// transaction processing.
func (t *jsTracer) OnTxEnd(receipt *types.Receipt, err error) {
	if t.err != nil {
		return
	}
	if err != nil {
		// Don't override vm error
		if _, ok := t.ctx["error"]; !ok {
			t.ctx["error"] = t.vm.ToValue(err.Error())
		}
		return
	}
	if receipt != nil {
		t.ctx["gasUsed"] = t.vm.ToValue(receipt.GasUsed)
	}
}

// onStart implements the Tracer interface to initialize the tracing operation.
func (t *jsTracer) onStart(from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	if t.err != nil {
		return
	}
	if create {
		t.ctx["type"] = t.vm.ToValue("CREATE")
	} else {
		t.ctx["type"] = t.vm.ToValue("CALL")
	}
	fromVal, err := t.toBuf(t.vm, from.Bytes())
	if err != nil {
		t.err = err
		return
	}
	t.ctx["from"] = fromVal
	toVal, err := t.toBuf(t.vm, to.Bytes())
	if err != nil {
		t.err = err
		return
	}
	t.ctx["to"] = toVal
	inputVal, err := t.toBuf(t.vm, input)
	if err != nil {
		t.err = err
		return
	}
	t.ctx["input"] = inputVal
	valueBig, err := t.toBig(t.vm, value.String())
	if err != nil {
		t.err = err
		return
	}
	t.ctx["value"] = valueBig
}

// OnOpcode implements the Tracer interface to trace a single step of VM execution.
func (t *jsTracer) OnOpcode(pc uint64, op byte, gas, cost uint64, scope tracing.OpContext, rData []byte, depth int, err error) {
	if !t.traceStep {
		return
	}
	if t.err != nil {
		return
	}

	log := t.log
	log.op.op = vm.OpCode(op)
	log.memory.memory = scope.MemoryData()
	log.stack.stack = scope.StackData()
	log.contract.scope = scope
	log.pc = pc
	log.gas = gas
	log.cost = cost
	log.refund = t.env.StateDB.GetRefund()
	log.depth = depth
	log.err = err
	if _, err := t.step(t.obj, t.logValue, t.dbValue); err != nil {
		t.onError("step", err)
	}
}

// OnFault implements the Tracer interface to trace an execution fault
func (t *jsTracer) OnFault(pc uint64, op byte, gas, cost uint64, scope tracing.OpContext, depth int, err error) {
	if t.err != nil {
		return
	}
	// Other log fields have been already set as part of the last OnOpcode.
	t.log.err = err
	if _, err := t.fault(t.obj, t.logValue, t.dbValue); err != nil {
		t.onError("fault", err)
	}
}

// onEnd is called after the call finishes to finalize the tracing.
func (t *jsTracer) onEnd(output []byte, gasUsed uint64, err error, reverted bool) {
	if t.err != nil {
		return
	}
	if err != nil {
		t.ctx["error"] = t.vm.ToValue(err.Error())
	}
	outputVal, err := t.toBuf(t.vm, output)
	if err != nil {
		t.err = err
		return
	}
	t.ctx["output"] = outputVal
}

// OnEnter is called when EVM enters a new scope (via call, create or selfdestruct).
func (t *jsTracer) OnEnter(depth int, typ byte, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	if t.err != nil {
		return
	}
	if depth == 0 {
		t.onStart(from, to, vm.OpCode(typ) == vm.CREATE, input, gas, value)
		return
	}
	if !t.traceFrame {
		return
	}

	t.frame.typ = vm.OpCode(typ).String()
	t.frame.from = from
	t.frame.to = to
	t.frame.input = common.CopyBytes(input)
	t.frame.gas = uint(gas)
	t.frame.value = nil
	if value != nil {
		t.frame.value = new(big.Int).SetBytes(value.Bytes())
	}

	if _, err := t.enter(t.obj, t.frameValue); err != nil {
		t.onError("enter", err)
	}
}

// OnExit is called when EVM exits a scope, even if the scope didn't
// execute any code.
func (t *jsTracer) OnExit(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
	if t.err != nil {
		return
	}
	if depth == 0 {
		t.onEnd(output, gasUsed, err, reverted)
		return
	}
	if !t.traceFrame {
		return
	}

	t.frameResult.gasUsed = uint(gasUsed)
	t.frameResult.output = common.CopyBytes(output)
	t.frameResult.err = err

	if _, err := t.exit(t.obj, t.frameResultValue); err != nil {
		t.onError("exit", err)
	}
}

// GetResult calls the Javascript 'result' function and returns its value, or any accumulated error
func (t *jsTracer) GetResult() (json.RawMessage, error) {
	if t.err != nil {
		return nil, t.err
	}
	ctx := t.vm.ToValue(t.ctx)
	res, err := t.result(t.obj, ctx, t.dbValue)
	if err != nil {
		return nil, wrapError("result", err)
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	return encoded, t.err
}

// Stop terminates execution of the tracer at the first opportune moment.
func (t *jsTracer) Stop(err error) {
	t.vm.Interrupt(err)
}

// onError is called anytime the running JS code is interrupted
// and returns an error. It in turn pings the EVM to cancel its
// execution.
func (t *jsTracer) onError(context string, err error) {
	t.err = wrapError(context, err)
}

func wrapError(context string, err error) error {
	return fmt.Errorf("%v    in server-side tracer function '%v'", err, context)
}

// setBuiltinFunctions injects Go functions which are available to tracers into the environment.
// It depends on type converters having been set up.
func (t *jsTracer) setBuiltinFunctions() {
	vm := t.vm
	// TODO: load console from goja-nodejs
	vm.Set("toHex", func(v goja.Value) string {
		b, err := t.fromBuf(vm, v, false)
		if err != nil {
			vm.Interrupt(err)
			return ""
		}
		return hexutil.Encode(b)
	})
	vm.Set("toWord", func(v goja.Value) goja.Value {
		// TODO: add test with []byte len < 32 or > 32
		b, err := t.fromBuf(vm, v, true)
		if err != nil {
			vm.Interrupt(err)
			return nil
		}
		b = common.BytesToHash(b).Bytes()
		res, err := t.toBuf(vm, b)
		if err != nil {
			vm.Interrupt(err)
			return nil
		}
		return res
	})
	vm.Set("toAddress", func(v goja.Value) goja.Value {
		a, err := t.fromBuf(vm, v, true)
		if err != nil {
			vm.Interrupt(err)
			return nil
		}
		a = common.BytesToAddress(a).Bytes()
		res, err := t.toBuf(vm, a)
		if err != nil {
			vm.Interrupt(err)
			return nil
		}
		return res
	})
	vm.Set("toContract", func(from goja.Value, nonce uint) goja.Value {
		a, err := t.fromBuf(vm, from, true)
		if err != nil {
			vm.Interrupt(err)
			return nil
		}
		addr := common.BytesToAddress(a)
		b := crypto.CreateAddress(addr, uint64(nonce)).Bytes()
		res, err := t.toBuf(vm, b)
		if err != nil {
			vm.Interrupt(err)
			return nil
		}
		return res
	})
	vm.Set("toContract2", func(from goja.Value, salt string, initcode goja.Value) goja.Value {
		a, err := t.fromBuf(vm, from, true)
		if err != nil {
			vm.Interrupt(err)
			return nil
		}
		addr := common.BytesToAddress(a)
		code, err := t.fromBuf(vm, initcode, true)
		if err != nil {
			vm.Interrupt(err)
			return nil
		}
		code = common.CopyBytes(code)
		codeHash := crypto.Keccak256(code)
		b := crypto.CreateAddress2(addr, common.HexToHash(salt), codeHash).Bytes()
		res, err := t.toBuf(vm, b)
		if err != nil {
			vm.Interrupt(err)
			return nil
		}
		return res
	})
	vm.Set("isPrecompiled", func(v goja.Value) bool {
		a, err := t.fromBuf(vm, v, true)
		if err != nil {
			vm.Interrupt(err)
			return false
		}
		return slices.Contains(t.activePrecompiles, common.BytesToAddress(a))
	})
	vm.Set("slice", func(slice goja.Value, start, end int64) goja.Value {
		b, err := t.fromBuf(vm, slice, false)
		if err != nil {
			vm.Interrupt(err)
			return nil
		}
		if start < 0 || start > end || end > int64(len(b)) {
			vm.Interrupt(fmt.Sprintf("Tracer accessed out of bound memory: available %d, offset %d, size %d", len(b), start, end-start))
			return nil
		}
		res, err := t.toBuf(vm, b[start:end])
		if err != nil {
			vm.Interrupt(err)
			return nil
		}
		return res
	})
}

// setTypeConverters sets up utilities for converting Go types into those
// suitable for JS consumption.
func (t *jsTracer) setTypeConverters() error {
	// Inject bigint logic.
	// TODO: To be replaced after goja adds support for native JS bigint.
	toBigCode, err := t.vm.RunProgram(getBigIntProgram())
	if err != nil {
		return err
	}
	// Used to create JS bigint objects from go.
	toBigFn, ok := goja.AssertFunction(toBigCode)
	if !ok {
		return errors.New("failed to bind bigInt func")
	}
	toBigWrapper := func(vm *goja.Runtime, val string) (goja.Value, error) {
		return toBigFn(goja.Undefined(), vm.ToValue(val))
	}
	t.toBig = toBigWrapper
	// NOTE: We need this workaround to create JS buffers because
	// goja doesn't at the moment expose constructors for typed arrays.
	//
	// Cache uint8ArrayType once to be used every time for less overhead.
	uint8ArrayType := t.vm.Get("Uint8Array")
	toBufWrapper := func(vm *goja.Runtime, val []byte) (goja.Value, error) {
		return toBuf(vm, uint8ArrayType, val)
	}
	t.toBuf = toBufWrapper
	fromBufWrapper := func(vm *goja.Runtime, buf goja.Value, allowString bool) ([]byte, error) {
		return fromBuf(vm, uint8ArrayType, buf, allowString)
	}
	t.fromBuf = fromBufWrapper
	return nil
}

type opObj struct {
	vm *goja.Runtime
	op vm.OpCode
}

func (o *opObj) ToNumber() int {
	return int(o.op)
}

func (o *opObj) ToString() string {
	return o.op.String()
}

func (o *opObj) IsPush() bool {
	return o.op.IsPush()
}

func (o *opObj) setupObject() *goja.Object {
	obj := o.vm.NewObject()
	obj.Set("toNumber", o.vm.ToValue(o.ToNumber))
	obj.Set("toString", o.vm.ToValue(o.ToString))
	obj.Set("isPush", o.vm.ToValue(o.IsPush))
	return obj
}

type memoryObj struct {
	memory []byte
	vm     *goja.Runtime
	toBig  toBigFn
	toBuf  toBufFn
}

func (mo *memoryObj) Slice(begin, end int64) goja.Value {
	b, err := mo.slice(begin, end)
	if err != nil {
		mo.vm.Interrupt(err)
		return nil
	}
	res, err := mo.toBuf(mo.vm, b)
	if err != nil {
		mo.vm.Interrupt(err)
		return nil
	}
	return res
}

// slice returns the requested range of memory as a byte slice.
func (mo *memoryObj) slice(begin, end int64) ([]byte, error) {
	if end == begin {
		return []byte{}, nil
	}
	if end < begin || begin < 0 {
		return nil, fmt.Errorf("tracer accessed out of bound memory: offset %d, end %d", begin, end)
	}
	slice, err := internal.GetMemoryCopyPadded(mo.memory, begin, end-begin)
	if err != nil {
		return nil, err
	}
	return slice, nil
}

func (mo *memoryObj) GetUint(addr int64) goja.Value {
	value, err := mo.getUint(addr)
	if err != nil {
		mo.vm.Interrupt(err)
		return nil
	}
	res, err := mo.toBig(mo.vm, value.String())
	if err != nil {
		mo.vm.Interrupt(err)
		return nil
	}
	return res
}

// getUint returns the 32 bytes at the specified address interpreted as a uint.
func (mo *memoryObj) getUint(addr int64) (*big.Int, error) {
	if len(mo.memory) < int(addr)+32 || addr < 0 {
		return nil, fmt.Errorf("tracer accessed out of bound memory: available %d, offset %d, size %d", len(mo.memory), addr, 32)
	}
	return new(big.Int).SetBytes(internal.MemoryPtr(mo.memory, addr, 32)), nil
}

func (mo *memoryObj) Length() int {
	return len(mo.memory)
}

func (mo *memoryObj) setupObject() *goja.Object {
	o := mo.vm.NewObject()
	o.Set("slice", mo.vm.ToValue(mo.Slice))
	o.Set("getUint", mo.vm.ToValue(mo.GetUint))
	o.Set("length", mo.vm.ToValue(mo.Length))
	return o
}

type stackObj struct {
	stack []uint256.Int
	vm    *goja.Runtime
	toBig toBigFn
}

func (s *stackObj) Peek(idx int) goja.Value {
	value, err := s.peek(idx)
	if err != nil {
		s.vm.Interrupt(err)
		return nil
	}
	res, err := s.toBig(s.vm, value.String())
	if err != nil {
		s.vm.Interrupt(err)
		return nil
	}
	return res
}

// peek returns the nth-from-the-top element of the stack.
func (s *stackObj) peek(idx int) (*big.Int, error) {
	if len(s.stack) <= idx || idx < 0 {
		return nil, fmt.Errorf("tracer accessed out of bound stack: size %d, index %d", len(s.stack), idx)
	}
	return internal.StackBack(s.stack, idx).ToBig(), nil
}

func (s *stackObj) Length() int {
	return len(s.stack)
}

func (s *stackObj) setupObject() *goja.Object {
	o := s.vm.NewObject()
	o.Set("peek", s.vm.ToValue(s.Peek))
	o.Set("length", s.vm.ToValue(s.Length))
	return o
}

type dbObj struct {
	db      tracing.StateDB
	vm      *goja.Runtime
	toBig   toBigFn
	toBuf   toBufFn
	fromBuf fromBufFn
}

func (do *dbObj) GetBalance(addrSlice goja.Value) goja.Value {
	a, err := do.fromBuf(do.vm, addrSlice, false)
	if err != nil {
		do.vm.Interrupt(err)
		return nil
	}
	addr := common.BytesToAddress(a)
	value := do.db.GetBalance(addr)
	res, err := do.toBig(do.vm, value.String())
	if err != nil {
		do.vm.Interrupt(err)
		return nil
	}
	return res
}

func (do *dbObj) GetNonce(addrSlice goja.Value) uint64 {
	a, err := do.fromBuf(do.vm, addrSlice, false)
	if err != nil {
		do.vm.Interrupt(err)
		return 0
	}
	addr := common.BytesToAddress(a)
	return do.db.GetNonce(addr)
}

func (do *dbObj) GetCode(addrSlice goja.Value) goja.Value {
	a, err := do.fromBuf(do.vm, addrSlice, false)
	if err != nil {
		do.vm.Interrupt(err)
		return nil
	}
	addr := common.BytesToAddress(a)
	code := do.db.GetCode(addr)
	res, err := do.toBuf(do.vm, code)
	if err != nil {
		do.vm.Interrupt(err)
		return nil
	}
	return res
}

func (do *dbObj) GetState(addrSlice goja.Value, hashSlice goja.Value) goja.Value {
	a, err := do.fromBuf(do.vm, addrSlice, false)
	if err != nil {
		do.vm.Interrupt(err)
		return nil
	}
	addr := common.BytesToAddress(a)
	h, err := do.fromBuf(do.vm, hashSlice, false)
	if err != nil {
		do.vm.Interrupt(err)
		return nil
	}
	hash := common.BytesToHash(h)
	state := do.db.GetState(addr, hash).Bytes()
	res, err := do.toBuf(do.vm, state)
	if err != nil {
		do.vm.Interrupt(err)
		return nil
	}
	return res
}

func (do *dbObj) Exists(addrSlice goja.Value) bool {
	a, err := do.fromBuf(do.vm, addrSlice, false)
	if err != nil {
		do.vm.Interrupt(err)
		return false
	}
	addr := common.BytesToAddress(a)
	return do.db.Exist(addr)
}

func (do *dbObj) setupObject() *goja.Object {
	o := do.vm.NewObject()
	o.Set("getBalance", do.vm.ToValue(do.GetBalance))
	o.Set("getNonce", do.vm.ToValue(do.GetNonce))
	o.Set("getCode", do.vm.ToValue(do.GetCode))
	o.Set("getState", do.vm.ToValue(do.GetState))
	o.Set("exists", do.vm.ToValue(do.Exists))
	return o
}

type contractObj struct {
	scope tracing.OpContext
	vm    *goja.Runtime
	toBig toBigFn
	toBuf toBufFn
}

func (co *contractObj) GetCaller() goja.Value {
	caller := co.scope.Caller().Bytes()
	res, err := co.toBuf(co.vm, caller)
	if err != nil {
		co.vm.Interrupt(err)
		return nil
	}
	return res
}

func (co *contractObj) GetAddress() goja.Value {
	addr := co.scope.Address().Bytes()
	res, err := co.toBuf(co.vm, addr)
	if err != nil {
		co.vm.Interrupt(err)
		return nil
	}
	return res
}

func (co *contractObj) GetValue() goja.Value {
	value := co.scope.CallValue()
	res, err := co.toBig(co.vm, value.String())
	if err != nil {
		co.vm.Interrupt(err)
		return nil
	}
	return res
}

func (co *contractObj) GetInput() goja.Value {
	input := common.CopyBytes(co.scope.CallInput())
	res, err := co.toBuf(co.vm, input)
	if err != nil {
		co.vm.Interrupt(err)
		return nil
	}
	return res
}

func (co *contractObj) setupObject() *goja.Object {
	o := co.vm.NewObject()
	o.Set("getCaller", co.vm.ToValue(co.GetCaller))
	o.Set("getAddress", co.vm.ToValue(co.GetAddress))
	o.Set("getValue", co.vm.ToValue(co.GetValue))
	o.Set("getInput", co.vm.ToValue(co.GetInput))
	return o
}

type callframe struct {
	vm    *goja.Runtime
	toBig toBigFn
	toBuf toBufFn

	typ   string
	from  common.Address
	to    common.Address
	input []byte
	gas   uint
	value *big.Int
}

func (f *callframe) GetType() string {
	return f.typ
}

func (f *callframe) GetFrom() goja.Value {
	from := f.from.Bytes()
	res, err := f.toBuf(f.vm, from)
	if err != nil {
		f.vm.Interrupt(err)
		return nil
	}
	return res
}

func (f *callframe) GetTo() goja.Value {
	to := f.to.Bytes()
	res, err := f.toBuf(f.vm, to)
	if err != nil {
		f.vm.Interrupt(err)
		return nil
	}
	return res
}

func (f *callframe) GetInput() goja.Value {
	input := f.input
	res, err := f.toBuf(f.vm, input)
	if err != nil {
		f.vm.Interrupt(err)
		return nil
	}
	return res
}

func (f *callframe) GetGas() uint {
	return f.gas
}

func (f *callframe) GetValue() goja.Value {
	if f.value == nil {
		return goja.Undefined()
	}
	res, err := f.toBig(f.vm, f.value.String())
	if err != nil {
		f.vm.Interrupt(err)
		return nil
	}
	return res
}

func (f *callframe) setupObject() *goja.Object {
	o := f.vm.NewObject()
	o.Set("getType", f.vm.ToValue(f.GetType))
	o.Set("getFrom", f.vm.ToValue(f.GetFrom))
	o.Set("getTo", f.vm.ToValue(f.GetTo))
	o.Set("getInput", f.vm.ToValue(f.GetInput))
	o.Set("getGas", f.vm.ToValue(f.GetGas))
	o.Set("getValue", f.vm.ToValue(f.GetValue))
	return o
}

type callframeResult struct {
	vm    *goja.Runtime
	toBuf toBufFn

	gasUsed uint
	output  []byte
	err     error
}

func (r *callframeResult) GetGasUsed() uint {
	return r.gasUsed
}

func (r *callframeResult) GetOutput() goja.Value {
	res, err := r.toBuf(r.vm, r.output)
	if err != nil {
		r.vm.Interrupt(err)
		return nil
	}
	return res
}

func (r *callframeResult) GetError() goja.Value {
	if r.err != nil {
		return r.vm.ToValue(r.err.Error())
	}
	return goja.Undefined()
}

func (r *callframeResult) setupObject() *goja.Object {
	o := r.vm.NewObject()
	o.Set("getGasUsed", r.vm.ToValue(r.GetGasUsed))
	o.Set("getOutput", r.vm.ToValue(r.GetOutput))
	o.Set("getError", r.vm.ToValue(r.GetError))
	return o
}

type steplog struct {
	vm *goja.Runtime

	op       *opObj
	memory   *memoryObj
	stack    *stackObj
	contract *contractObj

	pc     uint64
	gas    uint64
	cost   uint64
	depth  int
	refund uint64
	err    error
}

func (l *steplog) GetPC() uint64     { return l.pc }
func (l *steplog) GetGas() uint64    { return l.gas }
func (l *steplog) GetCost() uint64   { return l.cost }
func (l *steplog) GetDepth() int     { return l.depth }
func (l *steplog) GetRefund() uint64 { return l.refund }

func (l *steplog) GetError() goja.Value {
	if l.err != nil {
		return l.vm.ToValue(l.err.Error())
	}
	return goja.Undefined()
}

func (l *steplog) setupObject() *goja.Object {
	o := l.vm.NewObject()
	// Setup basic fields.
	o.Set("getPC", l.vm.ToValue(l.GetPC))
	o.Set("getGas", l.vm.ToValue(l.GetGas))
	o.Set("getCost", l.vm.ToValue(l.GetCost))
	o.Set("getDepth", l.vm.ToValue(l.GetDepth))
	o.Set("getRefund", l.vm.ToValue(l.GetRefund))
	o.Set("getError", l.vm.ToValue(l.GetError))
	// Setup nested objects.
	o.Set("op", l.op.setupObject())
	o.Set("stack", l.stack.setupObject())
	o.Set("memory", l.memory.setupObject())
	o.Set("contract", l.contract.setupObject())
	return o
}
