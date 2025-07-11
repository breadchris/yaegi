package interp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"go/build"
	"go/scanner"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Interpreter node structure for AST and CFG.
type node struct {
	debug      *nodeDebugData // debug info
	child      []*node        // child subtrees (AST)
	anc        *node          // ancestor (AST)
	param      []*itype       // generic parameter nodes (AST)
	start      *node          // entry point in subtree (CFG)
	tnext      *node          // true branch successor (CFG)
	fnext      *node          // false branch successor (CFG)
	interp     *Interpreter   // interpreter context
	index      int64          // node index (dot display)
	findex     int            // index of value in frame or frame size (func def, type def)
	level      int            // number of frame indirections to access value
	nleft      int            // number of children in left part (assign) or indicates preceding type (compositeLit)
	nright     int            // number of children in right part (assign)
	kind       nkind          // kind of node
	pos        token.Pos      // position in source code, relative to fset
	sym        *symbol        // associated symbol
	typ        *itype         // type of value in frame, or nil
	recv       *receiver      // method receiver node for call, or nil
	types      []reflect.Type // frame types, used by function literals only
	scope      *scope         // frame scope
	action     action         // action
	exec       bltn           // generated function to execute
	gen        bltnGenerator  // generator function to produce above bltn
	val        interface{}    // static generic value (CFG execution)
	rval       reflect.Value  // reflection value to let runtime access interpreter (CFG)
	ident      string         // set if node is a var or func
	redeclared bool           // set if node is a redeclared variable (CFG)
	meta       interface{}    // meta stores meta information between gta runs, like errors
}

func (n *node) shouldBreak() bool {
	if n == nil || n.debug == nil {
		return false
	}

	if n.debug.breakOnLine || n.debug.breakOnCall {
		return true
	}

	return false
}

func (n *node) setProgram(p *Program) {
	if n.debug == nil {
		n.debug = new(nodeDebugData)
	}
	n.debug.program = p
}

func (n *node) setBreakOnCall(v bool) {
	if n.debug == nil {
		if !v {
			return
		}
		n.debug = new(nodeDebugData)
	}
	n.debug.breakOnCall = v
}

func (n *node) setBreakOnLine(v bool) {
	if n.debug == nil {
		if !v {
			return
		}
		n.debug = new(nodeDebugData)
	}
	n.debug.breakOnLine = v
}

// receiver stores method receiver object access path.
type receiver struct {
	node  *node         // receiver value for alias and struct types
	val   reflect.Value // receiver value for interface type and value type
	index []int         // path in receiver value for interface or value type
}

// frame contains values for the current execution level (a function context).
type frame struct {
	// id is an atomic counter used for cancellation, only accessed
	// via newFrame/runid/setrunid/clone.
	// Located at start of struct to ensure proper alignment.
	id uint64

	debug *frameDebugData

	root *frame          // global space
	anc  *frame          // ancestor frame (caller space)
	data []reflect.Value // values

	mutex     sync.RWMutex
	deferred  [][]reflect.Value  // defer stack
	recovered interface{}        // to handle panic recover
	done      reflect.SelectCase // for cancellation of channel operations
}

func newFrame(anc *frame, length int, id uint64) *frame {
	f := &frame{
		anc:  anc,
		data: make([]reflect.Value, length),
		id:   id,
	}
	if anc == nil {
		f.root = f
	} else {
		f.done = anc.done
		f.root = anc.root
	}
	return f
}

func (f *frame) runid() uint64      { return atomic.LoadUint64(&f.id) }
func (f *frame) setrunid(id uint64) { atomic.StoreUint64(&f.id, id) }
func (f *frame) clone() *frame {
	f.mutex.RLock()
	defer f.mutex.RUnlock()
	nf := &frame{
		anc:       f.anc,
		root:      f.root,
		deferred:  f.deferred,
		recovered: f.recovered,
		id:        f.runid(),
		done:      f.done,
		debug:     f.debug,
	}
	nf.data = make([]reflect.Value, len(f.data))
	copy(nf.data, f.data)
	return nf
}

// Exports stores the map of binary packages per package path.
// The package path is the path joined from the import path and the package name
// as specified in source files by the "package" statement.
type Exports map[string]map[string]reflect.Value

// imports stores the map of source packages per package path.
type imports map[string]map[string]*symbol

// opt stores interpreter options.
type opt struct {
	// dotCmd is the command to process the dot graph produced when astDot and/or
	// cfgDot is enabled. It defaults to 'dot -Tdot -o <filename>.dot'.
	dotCmd       string
	context      build.Context     // build context: GOPATH, build constraints
	stdin        io.Reader         // standard input
	stdout       io.Writer         // standard output
	stderr       io.Writer         // standard error
	args         []string          // cmdline args
	env          map[string]string // environment of interpreter, entries in form of "key=value"
	filesystem   fs.FS             // filesystem containing sources
	astDot       bool              // display AST graph (debug)
	cfgDot       bool              // display CFG graph (debug)
	noRun        bool              // compile, but do not run
	fastChan     bool              // disable cancellable chan operations
	specialStdio bool              // allows os.Stdin, os.Stdout, os.Stderr to not be file descriptors
	unrestricted bool              // allow use of non-sandboxed symbols
}

// Interpreter contains global resources and state.
type Interpreter struct {
	// id is an atomic counter used for run cancellation,
	// only accessed via runid/stop
	// Located at start of struct to ensure proper alignment on 32-bit
	// architectures.
	id uint64

	// nindex is a node number incremented for each new node.
	// It is used for debug (AST and CFG graphs). As it is atomically
	// incremented, keep it aligned on 64 bits boundary.
	nindex int64

	name string // name of the input source file (or main)

	opt                                         // user settable options
	cancelChan bool                             // enables cancellable chan operations
	fset       *token.FileSet                   // fileset to locate node in source code
	binPkg     Exports                          // binary packages used in interpreter, indexed by path
	rdir       map[string]bool                  // for src import cycle detection
	mapTypes   map[reflect.Value][]reflect.Type // special interfaces mapping for wrappers

	mutex    sync.RWMutex
	frame    *frame            // program data storage during execution
	universe *scope            // interpreter global level scope
	scopes   map[string]*scope // package level scopes, indexed by import path
	srcPkg   imports           // source packages used in interpreter, indexed by path
	pkgNames map[string]string // package names, indexed by import path
	done     chan struct{}     // for cancellation of channel operations
	roots    []*node
	generic  map[string]*node

	hooks *hooks // symbol hooks

	debugger *Debugger
	calls    map[uintptr]*node // for translating runtime stacktrace, see FilterStack()
	panics   []*Panic          // list of panics we have had, see GetOldestPanicForErr()
}

const (
	mainID     = "main"
	selfPrefix = "github.com/breadchris/yaegi"
	selfPath   = selfPrefix + "/interp/interp"
	// DefaultSourceName is the name used by default when the name of the input
	// source file has not been specified for an Eval.
	// TODO(mpl): something even more special as a name?
	DefaultSourceName = "_.go"

	// Test is the value to pass to EvalPath to activate evaluation of test functions.
	Test = false
	// NoTest is the value to pass to EvalPath to skip evaluation of test functions.
	NoTest = true
)

// Self points to the current interpreter if accessed from within itself, or is nil.
var Self *Interpreter

// Symbols exposes interpreter values.
var Symbols = Exports{
	selfPath: map[string]reflect.Value{
		"New": reflect.ValueOf(New),

		"Interpreter": reflect.ValueOf((*Interpreter)(nil)),
		"Options":     reflect.ValueOf((*Options)(nil)),
		"Panic":       reflect.ValueOf((*Panic)(nil)),
		"IFunc":       reflect.ValueOf((*IFunc)(nil)),
	},
}

func init() { Symbols[selfPath]["Symbols"] = reflect.ValueOf(Symbols) }

// _error is a wrapper of error interface type.
type _error struct {
	IValue interface{}
	WError func() string
}

func (w _error) Error() string { return w.WError() }

// Walk traverses AST n in depth first order, call cbin function
// at node entry and cbout function at node exit.
func (n *node) Walk(in func(n *node) bool, out func(n *node)) {
	if in != nil && !in(n) {
		return
	}
	for _, child := range n.child {
		child.Walk(in, out)
	}
	if out != nil {
		out(n)
	}
}

// Options are the interpreter options.
type Options struct {
	// GoPath sets GOPATH for the interpreter.
	GoPath string

	// BuildTags sets build constraints for the interpreter.
	BuildTags []string

	// Standard input, output and error streams.
	// They default to os.Stdin, os.Stdout and os.Stderr respectively.
	Stdin          io.Reader
	Stdout, Stderr io.Writer

	// Cmdline args, defaults to os.Args.
	Args []string

	// Environment of interpreter. Entries are in the form "key=values".
	Env []string

	// SourcecodeFilesystem is where the _sourcecode_ is loaded from and does
	// NOT affect the filesystem of scripts when they run.
	// It can be any fs.FS compliant filesystem (e.g. embed.FS, or fstest.MapFS for testing)
	// See example/fs/fs_test.go for an example.
	SourcecodeFilesystem fs.FS

	// Unrestricted allows to run non sandboxed stdlib symbols such as os/exec and environment
	Unrestricted bool
}

// New returns a new interpreter.
func New(options Options) *Interpreter {
	i := Interpreter{
		opt:      opt{context: build.Default, filesystem: &realFS{}, env: map[string]string{}},
		frame:    newFrame(nil, 0, 0),
		fset:     token.NewFileSet(),
		universe: initUniverse(),
		scopes:   map[string]*scope{},
		binPkg:   Exports{"": map[string]reflect.Value{"_error": reflect.ValueOf((*_error)(nil))}},
		mapTypes: map[reflect.Value][]reflect.Type{},
		srcPkg:   imports{},
		pkgNames: map[string]string{},
		rdir:     map[string]bool{},
		hooks:    &hooks{},
		calls:    map[uintptr]*node{},
		panics:   []*Panic{},
		generic:  map[string]*node{},
	}

	if i.opt.stdin = options.Stdin; i.opt.stdin == nil {
		i.opt.stdin = os.Stdin
	}

	if i.opt.stdout = options.Stdout; i.opt.stdout == nil {
		i.opt.stdout = os.Stdout
	}

	if i.opt.stderr = options.Stderr; i.opt.stderr == nil {
		i.opt.stderr = os.Stderr
	}

	if i.opt.args = options.Args; i.opt.args == nil {
		i.opt.args = os.Args
	}

	// unrestricted allows to use non sandboxed stdlib symbols and env.
	if options.Unrestricted {
		i.opt.unrestricted = true
	} else {
		for _, e := range options.Env {
			a := strings.SplitN(e, "=", 2)
			if len(a) == 2 {
				i.opt.env[a[0]] = a[1]
			} else {
				i.opt.env[a[0]] = ""
			}
		}
	}

	if options.SourcecodeFilesystem != nil {
		i.opt.filesystem = options.SourcecodeFilesystem
	}

	i.opt.context.GOPATH = options.GoPath
	if len(options.BuildTags) > 0 {
		i.opt.context.BuildTags = options.BuildTags
	}

	// astDot activates AST graph display for the interpreter
	i.opt.astDot, _ = strconv.ParseBool(os.Getenv("YAEGI_AST_DOT"))

	// cfgDot activates CFG graph display for the interpreter
	i.opt.cfgDot, _ = strconv.ParseBool(os.Getenv("YAEGI_CFG_DOT"))

	// dotCmd defines how to process the dot code generated whenever astDot and/or
	// cfgDot is enabled. It defaults to 'dot -Tdot -o<filename>.dot' where filename
	// is context dependent.
	i.opt.dotCmd = os.Getenv("YAEGI_DOT_CMD")

	// noRun disables the execution (but not the compilation) in the interpreter
	i.opt.noRun, _ = strconv.ParseBool(os.Getenv("YAEGI_NO_RUN"))

	// fastChan disables the cancellable version of channel operations in evalWithContext
	i.opt.fastChan, _ = strconv.ParseBool(os.Getenv("YAEGI_FAST_CHAN"))

	// specialStdio allows to assign directly io.Writer and io.Reader to os.Stdxxx,
	// even if they are not file descriptors.
	i.opt.specialStdio, _ = strconv.ParseBool(os.Getenv("YAEGI_SPECIAL_STDIO"))

	return &i
}

const (
	bltnAlignof  = "unsafe.Alignof"
	bltnAppend   = "append"
	bltnCap      = "cap"
	bltnClose    = "close"
	bltnComplex  = "complex"
	bltnImag     = "imag"
	bltnCopy     = "copy"
	bltnDelete   = "delete"
	bltnLen      = "len"
	bltnMake     = "make"
	bltnNew      = "new"
	bltnOffsetof = "unsafe.Offsetof"
	bltnPanic    = "panic"
	bltnPrint    = "print"
	bltnPrintln  = "println"
	bltnReal     = "real"
	bltnRecover  = "recover"
	bltnSizeof   = "unsafe.Sizeof"
)

func initUniverse() *scope {
	sc := &scope{global: true, sym: map[string]*symbol{
		// predefined Go types
		"any":         {kind: typeSym, typ: &itype{cat: interfaceT, str: "any"}},
		"bool":        {kind: typeSym, typ: &itype{cat: boolT, name: "bool", str: "bool"}},
		"byte":        {kind: typeSym, typ: &itype{cat: uint8T, name: "uint8", str: "uint8"}},
		"comparable":  {kind: typeSym, typ: &itype{cat: comparableT, name: "comparable", str: "comparable"}},
		"complex64":   {kind: typeSym, typ: &itype{cat: complex64T, name: "complex64", str: "complex64"}},
		"complex128":  {kind: typeSym, typ: &itype{cat: complex128T, name: "complex128", str: "complex128"}},
		"error":       {kind: typeSym, typ: &itype{cat: errorT, name: "error", str: "error"}},
		"float32":     {kind: typeSym, typ: &itype{cat: float32T, name: "float32", str: "float32"}},
		"float64":     {kind: typeSym, typ: &itype{cat: float64T, name: "float64", str: "float64"}},
		"int":         {kind: typeSym, typ: &itype{cat: intT, name: "int", str: "int"}},
		"int8":        {kind: typeSym, typ: &itype{cat: int8T, name: "int8", str: "int8"}},
		"int16":       {kind: typeSym, typ: &itype{cat: int16T, name: "int16", str: "int16"}},
		"int32":       {kind: typeSym, typ: &itype{cat: int32T, name: "int32", str: "int32"}},
		"int64":       {kind: typeSym, typ: &itype{cat: int64T, name: "int64", str: "int64"}},
		"interface{}": {kind: typeSym, typ: &itype{cat: interfaceT, str: "interface{}"}},
		"rune":        {kind: typeSym, typ: &itype{cat: int32T, name: "int32", str: "int32"}},
		"string":      {kind: typeSym, typ: &itype{cat: stringT, name: "string", str: "string"}},
		"uint":        {kind: typeSym, typ: &itype{cat: uintT, name: "uint", str: "uint"}},
		"uint8":       {kind: typeSym, typ: &itype{cat: uint8T, name: "uint8", str: "uint8"}},
		"uint16":      {kind: typeSym, typ: &itype{cat: uint16T, name: "uint16", str: "uint16"}},
		"uint32":      {kind: typeSym, typ: &itype{cat: uint32T, name: "uint32", str: "uint32"}},
		"uint64":      {kind: typeSym, typ: &itype{cat: uint64T, name: "uint64", str: "uint64"}},
		"uintptr":     {kind: typeSym, typ: &itype{cat: uintptrT, name: "uintptr", str: "uintptr"}},

		// predefined Go constants
		"false": {kind: constSym, typ: untypedBool(nil), rval: reflect.ValueOf(false)},
		"true":  {kind: constSym, typ: untypedBool(nil), rval: reflect.ValueOf(true)},
		"iota":  {kind: constSym, typ: untypedInt(nil)},

		// predefined Go zero value
		"nil": {typ: &itype{cat: nilT, untyped: true, str: "nil"}},

		// predefined Go builtins
		bltnAppend:  {kind: bltnSym, builtin: _append},
		bltnCap:     {kind: bltnSym, builtin: _cap},
		bltnClose:   {kind: bltnSym, builtin: _close},
		bltnComplex: {kind: bltnSym, builtin: _complex},
		bltnImag:    {kind: bltnSym, builtin: _imag},
		bltnCopy:    {kind: bltnSym, builtin: _copy},
		bltnDelete:  {kind: bltnSym, builtin: _delete},
		bltnLen:     {kind: bltnSym, builtin: _len},
		bltnMake:    {kind: bltnSym, builtin: _make},
		bltnNew:     {kind: bltnSym, builtin: _new},
		bltnPanic:   {kind: bltnSym, builtin: _panic},
		bltnPrint:   {kind: bltnSym, builtin: _print},
		bltnPrintln: {kind: bltnSym, builtin: _println},
		bltnReal:    {kind: bltnSym, builtin: _real},
		bltnRecover: {kind: bltnSym, builtin: _recover},
	}}
	return sc
}

// resizeFrame resizes the global frame of interpreter.
func (interp *Interpreter) resizeFrame() {
	l := len(interp.universe.types)
	b := len(interp.frame.data)
	if l-b <= 0 {
		return
	}
	data := make([]reflect.Value, l)
	copy(data, interp.frame.data)
	for j, t := range interp.universe.types[b:] {
		data[b+j] = reflect.New(t).Elem()
	}
	interp.frame.data = data
}

// Add a call with handle that we recognize and can filter from the stacktrace
// Need to make sure this never overlaps with real PCs from runtime.Callers
func (interp *Interpreter) addCall(n *node) uintptr {
	handle := reflect.ValueOf(n).Pointer()
	interp.calls[handle] = n
	return handle
}

// Return func name as it appears in go stacktraces
func funcName(n *node) string {
	if n.scope == nil || n.scope.def == nil {
		return ""
	}

	// Need to search ancestors for both funcDecl and pkgName
	pkgName := n.scope.pkgName
	anc := n.scope
	ancestors := []*scope{}
	funcDeclFound := false
	funcDeclIndex := 0
	for anc != nil && anc != anc.anc {
		ancestors = append(ancestors, anc)
		if anc.def != nil && anc.def.kind == funcDecl &&
			(anc.anc == nil || anc.anc.def != anc.def) {
			funcDeclFound = true
			funcDeclIndex = len(ancestors) - 1
		}
		if len(anc.pkgName) > 0 {
			pkgName = anc.pkgName
		}
		if len(pkgName) > 0 && funcDeclFound {
			break
		}
		anc = anc.anc
	}

	if n.scope.def.typ.recv != nil {
		recv := n.scope.def.typ.recv.str
		star := ""
		if recv[0] == '*' {
			star = "*"
			recv = recv[1:]
		}
		recv = strings.TrimPrefix(recv, pkgName+".")
		pkgName = fmt.Sprintf("%s.(%s%s)", pkgName, star, recv)
	}

	funcName := "<unknown>"
	switch n.scope.def.kind {
	case funcDecl:
		funcName = n.scope.def.child[1].ident
	case funcLit:
		counts := []int{}
		count := 0
		i := funcDeclIndex
		funcName = ancestors[i].def.child[1].ident
		for i > 0 {
			if ancestors[i].dfs(func(s *scope) dfsSignal {
				if s.def != nil && s.def.kind == funcLit &&
					(s.anc == nil || s.def != s.anc.def) {
					count += 1
				}
				if s == ancestors[i-1] {
					if s.def != nil && s.def.kind == funcLit &&
						(s.anc == nil || s.def != s.anc.def) {
						counts = append(counts, count)
						count = 0
					}
					i -= 1
					return dfsAbort
				}
				if s.def != nil && s.def.kind == funcLit {
					return dfsSibling
				}
				return dfsNext
			}) != dfsAbort {
				// child not found
				return "<unknown>"
			}
		}
		funcName += fmt.Sprintf(".func%d", counts[0])
		for _, count := range counts[1:] {
			funcName += fmt.Sprintf(".%d", count)
			i += 1
		}
	}
	return fmt.Sprintf("%s.%s", pkgName, funcName)
}

// by analogy to runtime.FuncForPC()
type Func struct {
	Pos   token.Position
	name  string
	entry uintptr
}

func (f *Func) Entry() uintptr {
	return f.entry
}

func (f *Func) FileLine(pc uintptr) (string, int) {
	return f.Pos.Filename, f.Pos.Line
}

func (f *Func) Name() string {
	return f.name
}

type IFunc interface {
	Entry() uintptr
	FileLine(uintptr) (string, int)
	Name() string
}

// return call if we know it, pass to runtime.FuncForPC otherwise
func (interp *Interpreter) FuncForPC(handle uintptr) IFunc {
	n, ok := interp.calls[handle]
	if !ok {
		return runtime.FuncForPC(handle)
	}
	pos := n.interp.fset.Position(n.pos)
	return &Func{
		pos,
		funcName(n),
		handle,
	}
}

func (interp *Interpreter) FilteredStack() []byte {
	return interp.FilterStack(debug.Stack())
}

func (interp *Interpreter) FilteredCallers() []uintptr {
	pc := make([]uintptr, 64)
	runtime.Callers(0, pc)
	_, fPc := interp.FilterStackAndCallers(debug.Stack(), pc, 2)
	return fPc
}

func (interp *Interpreter) FilterStack(stack []byte) []byte {
	newStack, _ := interp.FilterStackAndCallers(stack, []uintptr{}, 2)
	return newStack
}

// Given a runtime stacktrace and callers list, filter out the interpreter runtime
// and replace it with the interpreted calls. Parses runtime stacktrace to figure
// out which interp node by placing a magic value in parameters to runCfg and callBin
func (interp *Interpreter) FilterStackAndCallers(stack []byte, callers []uintptr, skip int) ([]byte, []uintptr) {
	newFrames := [][]string{}
	newCallers := []uintptr{}

	stackLines := strings.Split(string(stack), "\n")
	lastFrame := len(stackLines)
	skipFrame := 0

	const (
		notSyncedYet = -1
		dontSync     = -2
	)

	// index to copy over from callers into newCallers
	callersIndex := notSyncedYet // to indicate we haven't synced up with stack yet
	if len(callers) == 0 {
		callersIndex = dontSync // don't attempt to copy over from callers
	}

	// Parse stack in reverse order, because sometimes we want to skip frames
	var lastInterpFrame int
	for i := len(stackLines) - 1; i >= 0; i-- {
		// Split stack trace into paragraphs (frames)
		if len(stackLines[i]) == 0 || stackLines[i][0] == '\t' {
			continue
		}

		if callersIndex > 0 {
			callersIndex--
		}

		if skipFrame > 0 {
			lastFrame = i
			skipFrame--
			continue
		}

		p := stackLines[i:lastFrame] // all lines in single frame
		lastFrame = i

		lastSlash := strings.LastIndex(p[0], "/")
		funcPath := strings.Split(p[0][lastSlash+1:], ".")
		pkgName := p[0][0:lastSlash+1] + funcPath[0]

		if callersIndex >= 0 {
			callName := runtime.FuncForPC(callers[callersIndex]).Name()
			if callName != strings.Split(p[0], "(")[0] {
				// for some reason runtime.gopanic shows up as panic in stacktrace
				if callName != "runtime.gopanic" || strings.Split(p[0], "(")[0] != "panic" {
					// since we're walking stack and callers at the same time they
					// should be in sync. If not, we stop messing with callers
					for ; callersIndex >= 0; callersIndex-- {
						newCallers = append(newCallers, callers[callersIndex])
					}
					callersIndex = dontSync
				}
			}
		}

		// Don't touch any stack frames that aren't in the yaegi runtime
		// Functions called on (*Interpreter) may provide information
		// on how we entered yaegi, so we pass these through as well
		if pkgName != selfPrefix+"/interp" || funcPath[1] == "(*Interpreter)" {
			newFrames = append(newFrames, p)
			if callersIndex >= 0 {
				newCallers = append(newCallers, callers[callersIndex])
			}
			continue
		}

		// This is the first call into the interpreter, so try to sync callers
		if callersIndex == notSyncedYet {
			for j, call := range callers {
				if call == 0 {
					break
				}
				callName := runtime.FuncForPC(call).Name()
				if callName == strings.Split(p[0], "(")[0] {
					callersIndex = j
				}
			}
			for j := len(callers) - 1; j > callersIndex; j-- {
				if callers[j] != 0 {
					newCallers = append(newCallers, callers[j])
				}
			}
		}

		var handle uintptr
		originalExecNode := false

		// A runCfg call refers to an interpreter level call
		// grab callHandle from the first parameter to it
		if strings.HasPrefix(funcPath[1], "runCfg(") {
			fmt.Sscanf(funcPath[1], "runCfg(%v,", &handle)
		}

		// capture node that panicked
		if strings.HasPrefix(funcPath[1], "runCfgPanic(") {
			fmt.Sscanf(funcPath[1], "runCfgPanic(%v,", &handle)
			originalExecNode = true
		}

		// callBin is a call to a binPkg
		// the callHandle will be on the first or second function literal
		if funcPath[1] == "callBin" &&
			(strings.HasPrefix(funcPath[2], "func1(") ||
				strings.HasPrefix(funcPath[2], "func2(")) {
			fmt.Sscanf(strings.Split(funcPath[2], "(")[1], "%v,", &handle)
			// after a binary call, the next two frames will be reflect.Value.Call
			skipFrame = 2
		}

		if handle != 0 {
			if callersIndex >= 0 {
				newCallers = append(newCallers, handle)
			}
			n, ok := interp.calls[handle]

			// Don't print scopes that weren't function calls
			// (unless they're the node that caused the panic)
			if !ok || (n.kind != callExpr && !originalExecNode) {
				continue
			}

			pos := n.interp.fset.Position(n.pos)
			newFrame := []string{
				funcName(n) + "()",
				fmt.Sprintf("\t%s", pos),
			}

			// we only find originalExecNode a few frames later
			// so place it right after the last interpreted frame
			if originalExecNode && len(newFrames) != lastInterpFrame {
				newFrames = append(
					newFrames[:lastInterpFrame+1],
					newFrames[lastInterpFrame:]...)
				newFrames[lastInterpFrame] = newFrame
			} else {
				newFrames = append(newFrames, newFrame)
			}
			lastInterpFrame = len(newFrames)
		}
	}

	// reverse order because we parsed from bottom up, fix that now.
	newStack := []string{}
	newStack = append(newStack, newFrames[len(newFrames)-1]...) // skip after goroutine id
	for i := len(newFrames) - 2 - skip; i >= 0; i-- {
		newStack = append(newStack, newFrames[i]...)
	}
	unreversedNewCallers := []uintptr{}
	if len(newCallers) == 0 {
		if len(callers) >= skip {
			unreversedNewCallers = callers[skip:] // just pass the original through
		}
	} else {
		for i := len(newCallers) - 1 - skip; i >= 0; i-- {
			unreversedNewCallers = append(unreversedNewCallers, newCallers[i])
		}
	}

	newStackJoined := strings.Join(newStack, "\n")
	newStackBytes := make([]byte, len(newStackJoined)-1)
	copy(newStackBytes, newStackJoined)
	return newStackBytes, unreversedNewCallers
}

// Panic is an error recovered from a panic call in interpreted code.
type Panic struct {
	// Value is the recovered value of a call to panic.
	Value interface{}

	// Callers is the call stack obtained from the recover call.
	// It may be used as the parameter to runtime.CallersFrames.
	Callers []uintptr

	// Stack is the call stack buffer for debug.
	Stack []byte

	// Interpreter runtime frames replaced by interpreted code
	FilteredCallers []uintptr
	FilteredStack   []byte
}

func (e Panic) Error() string {
	return fmt.Sprintf("panic: %s\n%s\n", e.Value, e.FilteredStack)
}

// Store a panic record if this is an error we have not seen.
// Not strictly correct: code might recover from err and never
// call GetOldestPanicForErr(), and we later return the wrong one.
func (interp *Interpreter) Panic(err interface{}) {
	if len(interp.panics) > 0 && interp.panics[len(interp.panics)-1].Value == err {
		return
	}
	pc := make([]uintptr, 64)
	runtime.Callers(0, pc)
	stack := debug.Stack()
	fStack, fPc := interp.FilterStackAndCallers(stack, pc, 2)
	interp.panics = append(interp.panics, &Panic{
		Value:           err,
		Callers:         pc,
		Stack:           stack,
		FilteredCallers: fPc,
		FilteredStack:   fStack,
	})
}

// We want to capture the full stacktrace from where the panic originated.
// Return oldest panic that matches err. Then, clear out the list of panics.
func (interp *Interpreter) GetOldestPanicForErr(err interface{}) *Panic {
	if _, ok := err.(*Panic); ok {
		return err.(*Panic)
	}
	r := (*Panic)(nil)
	for i := len(interp.panics) - 1; i >= 0; i-- {
		if interp.panics[i].Value == err {
			r = interp.panics[i]
			break
		}
	}
	interp.panics = []*Panic{}
	return r
}

// Eval evaluates Go code represented as a string. Eval returns the last result
// computed by the interpreter, and a non nil error in case of failure.
func (interp *Interpreter) Eval(src string) (res reflect.Value, err error) {
	return interp.eval(src, "", true)
}

// EvalPath evaluates Go code located at path and returns the last result computed
// by the interpreter, and a non nil error in case of failure.
// The main function of the main package is executed if present.
func (interp *Interpreter) EvalPath(path string) (res reflect.Value, err error) {
	path = filepath.ToSlash(path) // Ensure path is in Unix format. Since we work with fs.FS, we need to use Unix path.
	if !isFile(interp.opt.filesystem, path) {
		_, err := interp.importSrc(mainID, path, NoTest)
		return res, err
	}

	b, err := fs.ReadFile(interp.filesystem, path)
	if err != nil {
		return res, err
	}
	return interp.eval(string(b), path, false)
}

// EvalPathWithContext evaluates Go code located at path and returns the last
// result computed by the interpreter, and a non nil error in case of failure.
// The main function of the main package is executed if present.
func (interp *Interpreter) EvalPathWithContext(ctx context.Context, path string) (res reflect.Value, err error) {
	interp.mutex.Lock()
	interp.done = make(chan struct{})
	interp.cancelChan = !interp.opt.fastChan
	interp.mutex.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err = interp.EvalPath(path)
	}()

	select {
	case <-ctx.Done():
		interp.stop()
		return reflect.Value{}, ctx.Err()
	case <-done:
	}
	return res, err
}

// EvalTest evaluates Go code located at path, including test files with "_test.go" suffix.
// A non nil error is returned in case of failure.
// The main function, test functions and benchmark functions are internally compiled but not
// executed. Test functions can be retrieved using the Symbol() method.
func (interp *Interpreter) EvalTest(path string) error {
	_, err := interp.importSrc(mainID, path, Test)
	return err
}

func isFile(filesystem fs.FS, path string) bool {
	fi, err := fs.Stat(filesystem, path)
	return err == nil && fi.Mode().IsRegular()
}

func (interp *Interpreter) eval(src, name string, inc bool) (res reflect.Value, err error) {
	prog, err := interp.compileSrc(src, name, inc)
	if err != nil {
		return res, err
	}

	if interp.noRun {
		return res, err
	}

	return interp.Execute(prog)
}

// EvalWithContext evaluates Go code represented as a string. It returns
// a map on current interpreted package exported symbols.
func (interp *Interpreter) EvalWithContext(ctx context.Context, src string) (reflect.Value, error) {
	var v reflect.Value
	var err error

	interp.mutex.Lock()
	interp.done = make(chan struct{})
	interp.cancelChan = !interp.opt.fastChan
	interp.mutex.Unlock()

	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				var pc [64]uintptr
				n := runtime.Callers(1, pc[:])
				err = Panic{Value: r, Callers: pc[:n], Stack: debug.Stack()}
			}
			close(done)
		}()
		v, err = interp.Eval(src)
	}()

	select {
	case <-ctx.Done():
		interp.stop()
		return reflect.Value{}, ctx.Err()
	case <-done:
	}
	return v, err
}

// stop sends a semaphore to all running frames and closes the chan
// operation short circuit channel. stop may only be called once per
// invocation of EvalWithContext.
func (interp *Interpreter) stop() {
	atomic.AddUint64(&interp.id, 1)
	close(interp.done)
}

func (interp *Interpreter) runid() uint64 { return atomic.LoadUint64(&interp.id) }

// ignoreScannerError returns true if the error from Go scanner can be safely ignored
// to let the caller grab one more line before retrying to parse its input.
func ignoreScannerError(e *scanner.Error, s string) bool {
	msg := e.Msg
	if strings.HasSuffix(msg, "found 'EOF'") {
		return true
	}
	if msg == "raw string literal not terminated" {
		return true
	}
	if strings.HasPrefix(msg, "expected operand, found '}'") && !strings.HasSuffix(s, "}") {
		return true
	}
	return false
}

// ImportUsed automatically imports pre-compiled packages included by Use().
// This is mainly useful for REPLs, or single command lines. In case of an ambiguous default
// package name, for example "rand" for crypto/rand and math/rand, the package name is
// constructed by replacing the last "/" by a "_", producing crypto_rand and math_rand.
// ImportUsed should not be called more than once, and not after a first Eval, as it may
// rename packages.
func (interp *Interpreter) ImportUsed() {
	sc := interp.universe
	for k := range interp.binPkg {
		// By construction, the package name is the last path element of the key.
		name := path.Base(k)
		if sym, ok := sc.sym[name]; ok {
			// Handle collision by renaming old and new entries.
			name2 := key2name(fixKey(sym.typ.path))
			sc.sym[name2] = sym
			if name2 != name {
				delete(sc.sym, name)
			}
			name = key2name(fixKey(k))
		}
		sc.sym[name] = &symbol{kind: pkgSym, typ: &itype{cat: binPkgT, path: k, scope: sc}}
	}
}

func key2name(name string) string {
	return path.Join(name, DefaultSourceName)
}

func fixKey(k string) string {
	i := strings.LastIndex(k, "/")
	if i >= 0 {
		k = k[:i] + "_" + k[i+1:]
	}
	return k
}

// REPL performs a Read-Eval-Print-Loop on input reader.
// Results are printed to the output writer of the Interpreter, provided as option
// at creation time. Errors are printed to the similarly defined errors writer.
// The last interpreter result value and error are returned.
func (interp *Interpreter) REPL() (reflect.Value, error) {
	in, out, errs := interp.stdin, interp.stdout, interp.stderr
	ctx, cancel := context.WithCancel(context.Background())
	end := make(chan struct{})     // channel to terminate the REPL
	sig := make(chan os.Signal, 1) // channel to trap interrupt signal (Ctrl-C)
	lines := make(chan string)     // channel to read REPL input lines
	prompt := getPrompt(in, out)   // prompt activated on tty like IO stream
	s := bufio.NewScanner(in)      // read input stream line by line
	var v reflect.Value            // result value from eval
	var err error                  // error from eval
	src := ""                      // source string to evaluate

	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	prompt(v)

	go func() {
		defer close(end)
		for s.Scan() {
			lines <- s.Text()
		}
		if e := s.Err(); e != nil {
			fmt.Fprintln(errs, e)
		}
	}()

	go func() {
		for {
			select {
			case <-sig:
				cancel()
				lines <- ""
			case <-end:
				return
			}
		}
	}()

	for {
		var line string

		select {
		case <-end:
			cancel()
			return v, err
		case line = <-lines:
			src += line + "\n"
		}

		v, err = interp.EvalWithContext(ctx, src)
		if err != nil {
			switch e := err.(type) {
			case scanner.ErrorList:
				if len(e) > 0 && ignoreScannerError(e[0], line) {
					continue
				}
				fmt.Fprintln(errs, strings.TrimPrefix(e[0].Error(), DefaultSourceName+":"))
			case Panic:
				fmt.Fprintln(errs, e.Value)
				fmt.Fprintln(errs, string(e.Stack))
			default:
				fmt.Fprintln(errs, err)
			}
		}
		if errors.Is(err, context.Canceled) {
			ctx, cancel = context.WithCancel(context.Background())
		}
		src = ""
		prompt(v)
	}
}

func doPrompt(out io.Writer) func(v reflect.Value) {
	return func(v reflect.Value) {
		if v.IsValid() {
			fmt.Fprintln(out, ":", v)
		}
		fmt.Fprint(out, "> ")
	}
}

// getPrompt returns a function which prints a prompt only if input is a terminal.
func getPrompt(in io.Reader, out io.Writer) func(reflect.Value) {
	forcePrompt, _ := strconv.ParseBool(os.Getenv("YAEGI_PROMPT"))
	if forcePrompt {
		return doPrompt(out)
	}
	s, ok := in.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return func(reflect.Value) {}
	}
	stat, err := s.Stat()
	if err == nil && stat.Mode()&os.ModeCharDevice != 0 {
		return doPrompt(out)
	}
	return func(reflect.Value) {}
}
