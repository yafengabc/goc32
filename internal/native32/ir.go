package native32

// ir.go parses the textual LLVM IR that gocl's pure-Go IR translator emits.
// We do not load libLLVM: the text is the compiler's intermediate
// representation and the RV32/Thumb emitters consume it directly.
//
// The parser understands the subset gocl produces for freestanding C:
// globals, functions, allocas, loads/stores, integer arithmetic, GEP,
// comparisons, branches, phi, select, casts, calls (including the
// llvm.va_* / memcpy / memset intrinsics), switch and ret.

import (
	"fmt"
	"strconv"
	"strings"
)

type TypeKind int

const (
	KVoid TypeKind = iota
	KInt
	KPtr
	KArray
	KStruct
	KFunc
	KLabel
	KFloat
	KDouble
)

type Type struct {
	Kind     TypeKind
	Bits     int
	Elem     *Type
	N        int
	Fields   []*Type
	Ret      *Type
	Params   []*Type
	Variadic bool
}

func (t *Type) Size() int {
	if t == nil {
		return 0
	}
	switch t.Kind {
	case KInt:
		if t.Bits <= 8 {
			return 1
		}
		return (t.Bits + 7) / 8
	case KPtr, KFunc:
		return 4
	case KArray:
		return t.Elem.Size() * t.N
	case KStruct:
		off := 0
		al := 1
		for _, f := range t.Fields {
			if a := f.Align(); a > al {
				al = a
			}
			off = align(off, f.Align()) + f.Size()
		}
		return align(off, al)
	case KFloat:
		return 4
	case KDouble:
		return 8
	}
	return 0
}

func (t *Type) Align() int {
	if t == nil {
		return 1
	}
	switch t.Kind {
	case KInt:
		if s := t.Size(); s > 0 {
			return s
		}
		return 1
	case KPtr, KFunc:
		return 4
	case KArray:
		return t.Elem.Align()
	case KStruct:
		al := 1
		for _, f := range t.Fields {
			if a := f.Align(); a > al {
				al = a
			}
		}
		return al
	case KFloat:
		return 4
	case KDouble:
		return 8
	}
	return 1
}

func (t *Type) String() string {
	if t == nil {
		return "<nil>"
	}
	switch t.Kind {
	case KVoid:
		return "void"
	case KInt:
		return fmt.Sprintf("i%d", t.Bits)
	case KPtr:
		return "ptr"
	case KArray:
		return fmt.Sprintf("[%d x %s]", t.N, t.Elem)
	case KStruct:
		parts := make([]string, len(t.Fields))
		for i, f := range t.Fields {
			parts[i] = f.String()
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	case KFunc:
		return "func"
	case KLabel:
		return "label"
	case KFloat:
		return "float"
	case KDouble:
		return "double"
	}
	return "?"
}

func align(v, a int) int {
	if a <= 1 {
		return v
	}
	return (v + a - 1) &^ (a - 1)
}

// ---- values -------------------------------------------------------------

type ValKind int

const (
	VLocal ValKind = iota
	VGlobal
	VGlobalOff // @global + constant byte offset (a GEP in an initializer)
	VInt
	VNull
	VUndef
	VBool
	VZero
	VBytes
	VAgg
)

type Value struct {
	Kind  ValKind
	Name  string
	I     int64
	U     uint64
	B     []byte
	Elems []Value
	Ty    *Type
}

func (v Value) String() string {
	switch v.Kind {
	case VLocal:
		return "%" + v.Name
	case VGlobal:
		return "@" + v.Name
	case VGlobalOff:
		return fmt.Sprintf("@%s%+d", v.Name, v.I)
	case VInt:
		return strconv.FormatInt(v.I, 10)
	case VNull:
		return "null"
	case VUndef:
		return "undef"
	case VBool:
		if v.U != 0 {
			return "true"
		}
		return "false"
	case VZero:
		return "zeroinitializer"
	case VBytes:
		return fmt.Sprintf("%q", v.B)
	case VAgg:
		return "aggregate"
	}
	return "?"
}

type Global struct {
	Name    string
	Ty      *Type
	Init    Value
	Align   int
	Const   bool
	Private bool
}

type Param struct {
	Name string
	Ty   *Type
}

type Func struct {
	Name     string
	Ret      *Type
	Params   []Param
	Variadic bool
	Blocks   []*Block
	// Private is true for an `internal`/`private` IR definition, which comes
	// from a C `static` function.  It is safe to inline and remove such a
	// function: no other translation unit can name it.
	Private bool
}

type Block struct {
	Label string
	Insts []*Inst
}

type Case struct {
	Val  Value
	Dest string
}

type Inst struct {
	Op       string
	Dst      string
	Ty       *Type // result / loaded / allocated / destination type
	SrcTy    *Type // cast source
	Ops      []Value
	OpTys    []*Type
	Pred     string
	Labels   []string
	Cases    []Case
	Callee   string
	Indirect bool
	Align    int
}

type Module struct {
	Globals []*Global
	Funcs   []*Func
}

var namedTypes map[string]*Type

// ---- line scanner -------------------------------------------------------

func stripComment(s string) string {
	inStr := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inStr = !inStr
		case ';':
			if !inStr {
				return s[:i]
			}
		}
	}
	return s
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

func parseTypeAt(s string, i int) (*Type, int, error) {
	i = skipSpace(s, i)
	if i >= len(s) {
		return nil, i, fmt.Errorf("expected type at end of %q", s)
	}
	if s[i] == '%' {
		j := i + 1
		for j < len(s) && !isDelim(s[j]) {
			j++
		}
		name := s[i+1 : j]
		if t, ok := namedTypes[name]; ok {
			return t, j, nil
		}
		t := &Type{Kind: KStruct}
		namedTypes[name] = t
		return t, j, nil
	}
	switch s[i] {
	case '[':
		n, j, err := parseIntAt(s, i+1)
		if err != nil {
			return nil, i, err
		}
		j = skipSpace(s, j)
		if j >= len(s) || (s[j] != 'x' && s[j] != 'X') {
			return nil, i, fmt.Errorf("expected 'x' in array type %q", s[i:])
		}
		elem, j, err := parseTypeAt(s, j+1)
		if err != nil {
			return nil, i, err
		}
		j = skipSpace(s, j)
		if j >= len(s) || s[j] != ']' {
			return nil, i, fmt.Errorf("expected ']' in array type %q", s[i:])
		}
		return &Type{Kind: KArray, Elem: elem, N: int(n)}, j + 1, nil
	case '{':
		var fields []*Type
		j := i + 1
		for {
			j = skipSpace(s, j)
			if j < len(s) && s[j] == '}' {
				return &Type{Kind: KStruct, Fields: fields}, j + 1, nil
			}
			f, nj, err := parseTypeAt(s, j)
			if err != nil {
				return nil, i, err
			}
			fields = append(fields, f)
			j = skipSpace(s, nj)
			if j < len(s) && s[j] == ',' {
				j++
				continue
			}
		}
	case '<':
		// vector type: not produced for the C subset we target
		for i < len(s) && s[i] != '>' {
			i++
		}
		if i < len(s) {
			i++
		}
		return &Type{Kind: KArray, Elem: &Type{Kind: KInt, Bits: 8}, N: 0}, i, nil
	}
	j := i
	for j < len(s) && !isDelim(s[j]) {
		j++
	}
	tok := s[i:j]
	switch tok {
	case "void":
		return &Type{Kind: KVoid}, j, nil
	case "ptr":
		return &Type{Kind: KPtr}, j, nil
	case "label":
		return &Type{Kind: KLabel}, j, nil
	case "float":
		return &Type{Kind: KFloat}, j, nil
	case "double":
		return &Type{Kind: KDouble}, j, nil
	}
	if len(tok) > 1 && tok[0] == 'i' {
		n, err := strconv.Atoi(tok[1:])
		if err == nil {
			return &Type{Kind: KInt, Bits: n}, j, nil
		}
	}
	return nil, i, fmt.Errorf("unsupported type %q in %q", tok, s)
}

func isDelim(c byte) bool {
	switch c {
	case ' ', '\t', ',', ')', '(', ']', '[', '}', '{', '>', '<', '=':
		return true
	}
	return false
}

func parseIntAt(s string, i int) (int64, int, error) {
	i = skipSpace(s, i)
	j := i
	if j < len(s) && (s[j] == '-' || s[j] == '+') {
		j++
	}
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == i {
		return 0, i, fmt.Errorf("expected integer at %q", s[i:])
	}
	n, err := strconv.ParseInt(s[i:j], 10, 64)
	return n, j, err
}

func parseValueAt(s string, i int) (Value, int, error) {
	i = skipSpace(s, i)
	if i >= len(s) {
		return Value{}, i, fmt.Errorf("expected value at end of %q", s)
	}
	if strings.HasPrefix(s[i:], "getelementptr ") || strings.HasPrefix(s[i:], "getelementptr\t") {
		return parseGEPAt(s, i)
	}
	switch s[i] {
	case '%':
		j := i + 1
		for j < len(s) && !isDelim(s[j]) {
			j++
		}
		return Value{Kind: VLocal, Name: s[i+1 : j]}, j, nil
	case '@':
		j := i + 1
		for j < len(s) && !isDelim(s[j]) {
			j++
		}
		return Value{Kind: VGlobal, Name: s[i+1 : j]}, j, nil
	case 'c':
		if i+1 < len(s) && s[i+1] == '"' {
			j := i + 2
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return Value{}, i, fmt.Errorf("unterminated string")
			}
			raw := s[i+2 : j]
			b, err := unescapeIR(raw)
			if err != nil {
				return Value{}, i, err
			}
			return Value{Kind: VBytes, B: b}, j + 1, nil
		}
	case '[':
		var elems []Value
		j := i + 1
		for {
			j = skipSpace(s, j)
			if j < len(s) && s[j] == ']' {
				return Value{Kind: VAgg, Elems: elems}, j + 1, nil
			}
			// element is "<type> <value>"
			_, nj, err := parseTypeAt(s, j)
			if err != nil {
				return Value{}, i, err
			}
			v, nj2, err := parseValueAt(s, nj)
			if err != nil {
				return Value{}, i, err
			}
			elems = append(elems, v)
			j = skipSpace(s, nj2)
			if j < len(s) && s[j] == ',' {
				j++
				continue
			}
		}
	case '{':
		var elems []Value
		j := i + 1
		for {
			j = skipSpace(s, j)
			if j < len(s) && s[j] == '}' {
				return Value{Kind: VAgg, Elems: elems}, j + 1, nil
			}
			_, nj, err := parseTypeAt(s, j)
			if err != nil {
				return Value{}, i, err
			}
			v, nj2, err := parseValueAt(s, nj)
			if err != nil {
				return Value{}, i, err
			}
			elems = append(elems, v)
			j = skipSpace(s, nj2)
			if j < len(s) && s[j] == ',' {
				j++
				continue
			}
		}
	case 'z':
		if strings.HasPrefix(s[i:], "zeroinitializer") {
			return Value{Kind: VZero}, i + len("zeroinitializer"), nil
		}
	case 'n':
		if strings.HasPrefix(s[i:], "null") {
			return Value{Kind: VNull}, i + 4, nil
		}
	case 'u':
		if strings.HasPrefix(s[i:], "undef") {
			return Value{Kind: VUndef}, i + 5, nil
		}
	case 't':
		if strings.HasPrefix(s[i:], "true") {
			return Value{Kind: VBool, U: 1}, i + 4, nil
		}
	case 'f':
		if strings.HasPrefix(s[i:], "false") {
			return Value{Kind: VBool}, i + 5, nil
		}
	}
	// integer (decimal, negative, or 0x...)
	j := i
	if j < len(s) && (s[j] == '-' || s[j] == '+') {
		j++
	}
	start := j
	if j+1 < len(s) && s[j] == '0' && (s[j+1] == 'x' || s[j+1] == 'X') {
		j += 2
		for j < len(s) && isHex(s[j]) {
			j++
		}
		u, err := strconv.ParseUint(s[start:j], 0, 64)
		if err != nil {
			return Value{}, i, err
		}
		return Value{Kind: VInt, U: u, I: int64(u)}, j, nil
	}
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == start {
		return Value{}, i, fmt.Errorf("unsupported value at %q", s[i:])
	}
	u, err := strconv.ParseUint(s[start:j], 10, 64)
	if err != nil {
		return Value{}, i, err
	}
	if start > i && s[i] == '-' {
		return Value{Kind: VInt, I: -int64(u), U: uint64(-int64(u))}, j, nil
	}
	return Value{Kind: VInt, I: int64(u), U: u}, j, nil
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unescapeIR(s string) ([]byte, error) {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(s) {
			return nil, fmt.Errorf("bad escape")
		}
		switch s[i] {
		case '\\', '"':
			out = append(out, s[i])
		case 'n':
			out = append(out, '\n')
		case 't':
			out = append(out, '\t')
		case 'r':
			out = append(out, '\r')
		default:
			// LLVM spells bytes as two hex digits (\0A for newline, \00
			// for NUL). The hex form wins over any single-character
			// escape; '\0' alone is not a valid LLVM escape.
			if i+1 < len(s) && isHex(s[i]) && isHex(s[i+1]) {
				v, _ := strconv.ParseUint(s[i:i+2], 16, 8)
				out = append(out, byte(v))
				i++
			} else {
				out = append(out, s[i])
			}
		}
	}
	return out, nil
}

// splitTop splits on commas that are not nested in brackets, braces or
// parentheses, and not inside a string.
func splitTop(s string) []string {
	var out []string
	depth := 0
	inStr := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inStr = !inStr
		case '[', '{', '(':
			if !inStr {
				depth++
			}
		case ']', '}', ')':
			if !inStr {
				depth--
			}
		case ',':
			if !inStr && depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}

// parseGEPAt parses a constant getelementptr used as a global initializer and
// reduces it to symbol+offset. LLVM prints such a pointer as e.g.
//
//	getelementptr inbounds ([6 x i8], ptr @.const.0, i64 0, i64 0)
//
// which is just the address of the string; reloc.c has an array of these.
func parseGEPAt(s string, i int) (Value, int, error) {
	j := i + len("getelementptr")
	j = skipSpace(s, j)
	if strings.HasPrefix(s[j:], "inbounds") {
		j += len("inbounds")
		j = skipSpace(s, j)
	}
	// Constant-expression form: getelementptr inbounds (T, ptr @x, ...).
	if j < len(s) && s[j] == '(' {
		j++
	}
	cur, j, err := parseTypeAt(s, j)
	if err != nil {
		return Value{}, i, err
	}
	j = skipSpace(s, j)
	if j < len(s) && s[j] == ',' {
		j++
	}
	j = skipSpace(s, j)
	if strings.HasPrefix(s[j:], "ptr") {
		j += 3
	} else {
		if _, j, err = parseTypeAt(s, j); err != nil {
			return Value{}, i, err
		}
	}
	base, j, err := parseValueAt(s, j)
	if err != nil {
		return Value{}, i, err
	}
	if base.Kind != VGlobal {
		return Value{}, i, fmt.Errorf("GEP base is not a global: %s", base)
	}
	var off int64
	first := true
	for {
		j = skipSpace(s, j)
		if j >= len(s) || s[j] != ',' {
			break
		}
		// A GEP index is <integer type> <value>. The comma that separates
		// array elements is followed by the element type (usually "ptr");
		// leave that comma for the aggregate initializer parser.
		comma := j
		ty, nj, err := parseTypeAt(s, j+1)
		if err != nil || ty.Kind != KInt {
			j = comma
			break
		}
		j = nj
		idx, nj, err := parseValueAt(s, j)
		if err != nil {
			return Value{}, i, err
		}
		j = nj
		if idx.Kind != VInt {
			return Value{}, i, fmt.Errorf("non-constant GEP index %s", idx)
		}
		n := idx.I
		if first {
			off += n * int64(cur.Size())
			first = false
			continue
		}
		switch cur.Kind {
		case KArray:
			cur = cur.Elem
			off += n * int64(cur.Size())
		case KStruct:
			if n < 0 || int(n) >= len(cur.Fields) {
				return Value{}, i, fmt.Errorf("GEP field %d out of range", n)
			}
			fo := 0
			for k := 0; k < int(n); k++ {
				fo = align(fo, cur.Fields[k].Align()) + cur.Fields[k].Size()
			}
			off += int64(fo)
			cur = cur.Fields[n]
		default:
			off += n
		}
	}
	j = skipSpace(s, j)
	if j < len(s) && s[j] == ')' {
		j++
	}
	return Value{Kind: VGlobalOff, Name: base.Name, I: off}, j, nil
}

func ParseModule(src string) (*Module, error) {
	m := &Module{}
	namedTypes = map[string]*Type{}
	lines := strings.Split(src, "\n")
	var fn *Func
	var blk *Block
	for ln, raw := range lines {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "%") && strings.Contains(line, " = type ") {
			eq := strings.Index(line, " = type ")
			name := strings.TrimSpace(line[1:eq])
			ty, _, err := parseTypeAt(line[eq+len(" = type "):], 0)
			if err != nil {
				return nil, fmt.Errorf("line %d: named type %%%s: %v", ln+1, name, err)
			}
			if old, ok := namedTypes[name]; ok {
				*old = *ty
			} else {
				namedTypes[name] = ty
			}
			continue
		}
		if fn != nil {
			if line == "}" {
				m.Funcs = append(m.Funcs, fn)
				fn = nil
				blk = nil
				continue
			}
			if strings.HasSuffix(line, ":") && !strings.Contains(line, "=") {
				blk = &Block{Label: strings.TrimSuffix(line, ":")}
				fn.Blocks = append(fn.Blocks, blk)
				continue
			}
			if blk == nil {
				return nil, fmt.Errorf("line %d: instruction outside a block: %s", ln+1, line)
			}
			inst, err := parseInst(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %v: %s", ln+1, err, line)
			}
			blk.Insts = append(blk.Insts, inst)
			continue
		}
		switch {
		case strings.HasPrefix(line, "define "):
			f, err := parseFuncHeader(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %v: %s", ln+1, err, line)
			}
			fn = f
		case strings.HasPrefix(line, "declare "),
			strings.HasPrefix(line, "target "),
			strings.HasPrefix(line, "attributes "),
			strings.HasPrefix(line, "!"),
			strings.HasPrefix(line, "module asm"),
			strings.HasPrefix(line, "source_filename"):
			// not needed by the back end
		case strings.HasPrefix(line, "@"):
			g, err := parseGlobal(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %v: %s", ln+1, err, line)
			}
			if g != nil {
				m.Globals = append(m.Globals, g)
			}
		default:
			// Ignore anything else (LLVM metadata, comdat, ...).
		}
	}
	if fn != nil {
		return nil, fmt.Errorf("unterminated function %s", fn.Name)
	}
	simplifyModule(m)
	dceModule(m)
	return m, nil
}

func parseFuncHeader(line string) (*Func, error) {
	at := strings.Index(line, "@")
	if at < 0 {
		return nil, fmt.Errorf("no function name")
	}
	// return type is the token before @
	rstart := at - 1
	for rstart >= 0 && line[rstart] == ' ' {
		rstart--
	}
	rend := rstart + 1
	for rstart >= 0 && !isDelim(line[rstart]) {
		rstart--
	}
	retTok := line[rstart+1 : rend]
	ret, _, err := parseTypeAt(retTok, 0)
	if err != nil {
		return nil, fmt.Errorf("return type: %v", err)
	}
	lp := strings.Index(line[at:], "(")
	if lp < 0 {
		return nil, fmt.Errorf("no parameter list")
	}
	lp += at
	rp := matchParen(line, lp)
	if rp < 0 {
		return nil, fmt.Errorf("unterminated parameter list")
	}
	name := line[at+1 : lp]
	f := &Func{Name: name, Ret: ret}
	for _, tok := range strings.Fields(line[:at]) {
		if tok == "internal" || tok == "private" {
			f.Private = true
		}
	}
	params := strings.TrimSpace(line[lp+1 : rp])
	if params != "" {
		for _, p := range splitTop(params) {
			if strings.TrimSpace(p) == "..." {
				f.Variadic = true
				continue
			}
			ty, ni, err := parseTypeAt(p, 0)
			if err != nil {
				return nil, fmt.Errorf("parameter %q: %v", p, err)
			}
			ni = skipSpace(p, ni)
			if ni >= len(p) || p[ni] != '%' {
				return nil, fmt.Errorf("parameter %q has no name", p)
			}
			f.Params = append(f.Params, Param{Name: p[ni+1:], Ty: ty})
		}
	}
	return f, nil
}

func matchParen(s string, i int) int {
	depth := 0
	inStr := false
	for ; i < len(s); i++ {
		switch s[i] {
		case '"':
			inStr = !inStr
		case '(':
			if !inStr {
				depth++
			}
		case ')':
			if !inStr {
				depth--
				if depth == 0 {
					return i
				}
			}
		}
	}
	return -1
}

func parseGlobal(line string) (*Global, error) {
	eq := strings.Index(line, "=")
	if eq < 0 {
		return nil, nil
	}
	name := strings.TrimSpace(line[1:eq])
	rest := strings.TrimSpace(line[eq+1:])
	g := &Global{Name: name, Align: 1}
	for {
		if strings.HasPrefix(rest, "dso_local ") {
			rest = strings.TrimSpace(rest[len("dso_local "):])
			continue
		}
		if strings.HasPrefix(rest, "unnamed_addr ") {
			rest = strings.TrimSpace(rest[len("unnamed_addr "):])
			continue
		}
		if strings.HasPrefix(rest, "local_unnamed_addr ") {
			rest = strings.TrimSpace(rest[len("local_unnamed_addr "):])
			continue
		}
		if strings.HasPrefix(rest, "private ") {
			g.Private = true
			rest = strings.TrimSpace(rest[len("private "):])
			continue
		}
		if strings.HasPrefix(rest, "internal ") {
			g.Private = true
			rest = strings.TrimSpace(rest[len("internal "):])
			continue
		}
		break
	}
	if strings.HasPrefix(rest, "constant ") {
		g.Const = true
		rest = strings.TrimSpace(rest[len("constant "):])
	} else if strings.HasPrefix(rest, "global ") {
		rest = strings.TrimSpace(rest[len("global "):])
	} else if strings.HasPrefix(rest, "external ") {
		return nil, nil
	} else {
		return nil, fmt.Errorf("unsupported global definition")
	}
	ty, ni, err := parseTypeAt(rest, 0)
	if err != nil {
		return nil, err
	}
	g.Ty = ty
	v, ni2, err := parseValueAt(rest, ni)
	if err != nil {
		return nil, err
	}
	g.Init = v
	ni2 = skipSpace(rest, ni2)
	if strings.HasPrefix(rest[ni2:], ", align ") {
		n, _, err := parseIntAt(rest, ni2+len(", align "))
		if err == nil && n > 0 {
			g.Align = int(n)
		}
	}
	return g, nil
}

func parseInst(line string) (*Inst, error) {
	in := &Inst{}
	rest := line
	if strings.HasPrefix(rest, "tail ") {
		rest = strings.TrimSpace(rest[len("tail "):])
	}
	if strings.HasPrefix(rest, "%") {
		eq := strings.Index(rest, "=")
		if eq < 0 {
			return nil, fmt.Errorf("malformed assignment")
		}
		in.Dst = strings.TrimSpace(rest[1:eq])
		rest = strings.TrimSpace(rest[eq+1:])
	}
	space := strings.IndexByte(rest, ' ')
	if space < 0 {
		in.Op = rest
		switch in.Op {
		case "unreachable":
			return in, nil
		}
		return nil, fmt.Errorf("unsupported instruction %q", rest)
	}
	in.Op = rest[:space]
	rest = strings.TrimSpace(rest[space+1:])
	switch in.Op {
	case "alloca":
		rest = skipIRFlags(rest)
		ty, ni, err := parseTypeAt(rest, 0)
		if err != nil {
			return nil, err
		}
		in.Ty = ty
		ni = skipSpace(rest, ni)
		if strings.HasPrefix(rest[ni:], ", i32 ") {
			n, _, err := parseIntAt(rest, ni+len(", i32 "))
			if err == nil {
				in.Align = int(n)
			}
		}
	case "load":
		ty, ni, err := parseTypeAt(rest, 0)
		if err != nil {
			return nil, err
		}
		in.Ty = ty
		ni = skipSpace(rest, ni)
		if ni < len(rest) && rest[ni] == ',' {
			ni++
		}
		// optional "ptr" or a pointer type
		ni = skipSpace(rest, ni)
		if strings.HasPrefix(rest[ni:], "ptr ") {
			ni += 4
		} else if _, nj, err := parseTypeAt(rest, ni); err == nil {
			ni = nj
		}
		v, _, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ops = append(in.Ops, v)
		in.OpTys = append(in.OpTys, &Type{Kind: KPtr})
	case "store":
		ty, ni, err := parseTypeAt(rest, 0)
		if err != nil {
			return nil, err
		}
		in.Ty = ty
		v, ni, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ops = append(in.Ops, v)
		in.OpTys = append(in.OpTys, ty)
		ni = skipSpace(rest, ni)
		if ni < len(rest) && rest[ni] == ',' {
			ni++
		}
		ni = skipSpace(rest, ni)
		if strings.HasPrefix(rest[ni:], "ptr ") {
			ni += 4
		} else if _, nj, err := parseTypeAt(rest, ni); err == nil {
			ni = nj
		}
		p, _, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ops = append(in.Ops, p)
		in.OpTys = append(in.OpTys, &Type{Kind: KPtr})
	case "getelementptr":
		rest = skipIRFlags(rest)
		ty, ni, err := parseTypeAt(rest, 0)
		if err != nil {
			return nil, err
		}
		in.Ty = ty
		ni = skipSpace(rest, ni)
		if ni < len(rest) && rest[ni] == ',' {
			ni++
		}
		ni = skipSpace(rest, ni)
		if strings.HasPrefix(rest[ni:], "ptr ") {
			ni += 4
		} else if _, nj, err := parseTypeAt(rest, ni); err == nil {
			ni = nj
		}
		base, ni, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ops = append(in.Ops, base)
		in.OpTys = append(in.OpTys, &Type{Kind: KPtr})
		for {
			ni = skipSpace(rest, ni)
			if ni >= len(rest) || rest[ni] != ',' {
				break
			}
			ni++
			idxTy, nj, err := parseTypeAt(rest, ni)
			if err != nil {
				return nil, err
			}
			idx, nj2, err := parseValueAt(rest, nj)
			if err != nil {
				return nil, err
			}
			in.Ops = append(in.Ops, idx)
			in.OpTys = append(in.OpTys, idxTy)
			ni = nj2
		}
	case "add", "sub", "mul", "sdiv", "udiv", "srem", "urem",
		"shl", "lshr", "ashr", "and", "or", "xor":
		rest = skipIRFlags(rest)
		ty, ni, err := parseTypeAt(rest, 0)
		if err != nil {
			return nil, err
		}
		in.Ty = ty
		a, ni, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		ni = skipSpace(rest, ni)
		if ni < len(rest) && rest[ni] == ',' {
			ni++
		}
		b, _, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ops = []Value{a, b}
		in.OpTys = []*Type{ty, ty}
	case "icmp":
		ni := 0
		pred, ni, err := parseWordAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Pred = pred
		ty, ni, err := parseTypeAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ty = ty
		a, ni, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		ni = skipSpace(rest, ni)
		if ni < len(rest) && rest[ni] == ',' {
			ni++
		}
		b, _, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ops = []Value{a, b}
		in.OpTys = []*Type{ty, ty}
	case "br":
		ni := 0
		if strings.HasPrefix(rest, "label ") {
			v, _, err := parseValueAt(rest, len("label "))
			if err != nil {
				return nil, err
			}
			in.Labels = []string{v.Name}
			break
		}
		// br i1 %c, label %T, label %F
		ty, nj, err := parseTypeAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ty = ty
		c, nj, err := parseValueAt(rest, nj)
		if err != nil {
			return nil, err
		}
		in.Ops = []Value{c}
		in.OpTys = []*Type{ty}
		nj = skipSpace(rest, nj)
		if nj < len(rest) && rest[nj] == ',' {
			nj++
		}
		nj = skipSpace(rest, nj)
		if !strings.HasPrefix(rest[nj:], "label ") {
			return nil, fmt.Errorf("expected label")
		}
		t, nj, err := parseValueAt(rest, nj+len("label "))
		if err != nil {
			return nil, err
		}
		nj = skipSpace(rest, nj)
		if nj < len(rest) && rest[nj] == ',' {
			nj++
		}
		nj = skipSpace(rest, nj)
		if !strings.HasPrefix(rest[nj:], "label ") {
			return nil, fmt.Errorf("expected label")
		}
		f, _, err := parseValueAt(rest, nj+len("label "))
		if err != nil {
			return nil, err
		}
		in.Labels = []string{t.Name, f.Name}
	case "ret":
		if strings.HasPrefix(rest, "void") {
			in.Ty = &Type{Kind: KVoid}
			break
		}
		ty, ni, err := parseTypeAt(rest, 0)
		if err != nil {
			return nil, err
		}
		in.Ty = ty
		v, _, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ops = []Value{v}
		in.OpTys = []*Type{ty}
	case "trunc", "zext", "sext", "bitcast", "ptrtoint", "inttoptr",
		"uitofp", "sitofp", "fptoui", "fptosi", "fptrunc", "fpext":
		ty, ni, err := parseTypeAt(rest, 0)
		if err != nil {
			return nil, err
		}
		in.SrcTy = ty
		v, ni, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ops = []Value{v}
		in.OpTys = []*Type{ty}
		ni = skipSpace(rest, ni)
		if !strings.HasPrefix(rest[ni:], "to ") {
			return nil, fmt.Errorf("expected 'to'")
		}
		dt, _, err := parseTypeAt(rest, ni+3)
		if err != nil {
			return nil, err
		}
		in.Ty = dt
	case "select":
		ty, ni, err := parseTypeAt(rest, 0)
		if err != nil {
			return nil, err
		}
		in.Ty = ty
		in.OpTys = append(in.OpTys, ty)
		c, ni, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ops = append(in.Ops, c)
		ni = skipSpace(rest, ni)
		if ni < len(rest) && rest[ni] == ',' {
			ni++
		}
		aTy, ni, err := parseTypeAt(rest, ni)
		if err != nil {
			return nil, err
		}
		a, ni, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		ni = skipSpace(rest, ni)
		if ni < len(rest) && rest[ni] == ',' {
			ni++
		}
		bTy, ni, err := parseTypeAt(rest, ni)
		if err != nil {
			return nil, err
		}
		b, _, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ops = append(in.Ops, a, b)
		in.OpTys = append(in.OpTys, aTy, bTy)
	case "phi":
		ty, ni, err := parseTypeAt(rest, 0)
		if err != nil {
			return nil, err
		}
		in.Ty = ty
		for {
			ni = skipSpace(rest, ni)
			if ni >= len(rest) {
				break
			}
			if rest[ni] == ',' {
				ni++
				ni = skipSpace(rest, ni)
			}
			if ni >= len(rest) || rest[ni] != '[' {
				break
			}
			v, nj, err := parseValueAt(rest, ni+1)
			if err != nil {
				return nil, err
			}
			nj = skipSpace(rest, nj)
			if nj < len(rest) && rest[nj] == ',' {
				nj++
			}
			nj = skipSpace(rest, nj)
			lab, nj, err := parseValueAt(rest, nj)
			if err != nil {
				return nil, err
			}
			nj = skipSpace(rest, nj)
			if nj >= len(rest) || rest[nj] != ']' {
				return nil, fmt.Errorf("expected ']' in phi")
			}
			in.Ops = append(in.Ops, v)
			in.Labels = append(in.Labels, lab.Name)
			ni = nj + 1
		}
	case "switch":
		ty, ni, err := parseTypeAt(rest, 0)
		if err != nil {
			return nil, err
		}
		in.Ty = ty
		v, ni, err := parseValueAt(rest, ni)
		if err != nil {
			return nil, err
		}
		in.Ops = []Value{v}
		in.OpTys = []*Type{ty}
		ni = skipSpace(rest, ni)
		if ni < len(rest) && rest[ni] == ',' {
			ni++
		}
		ni = skipSpace(rest, ni)
		if !strings.HasPrefix(rest[ni:], "label ") {
			return nil, fmt.Errorf("expected default label")
		}
		def, ni, err := parseValueAt(rest, ni+len("label "))
		if err != nil {
			return nil, err
		}
		in.Labels = []string{def.Name}
		ni = skipSpace(rest, ni)
		if ni < len(rest) && rest[ni] == '[' {
			ni++
			for {
				ni = skipSpace(rest, ni)
				if ni >= len(rest) || rest[ni] == ']' {
					break
				}
				ty2, nj, err := parseTypeAt(rest, ni)
				if err != nil {
					return nil, err
				}
				cv, nj, err := parseValueAt(rest, nj)
				if err != nil {
					return nil, err
				}
				nj = skipSpace(rest, nj)
				if nj < len(rest) && rest[nj] == ',' {
					nj++
				}
				nj = skipSpace(rest, nj)
				if !strings.HasPrefix(rest[nj:], "label ") {
					return nil, fmt.Errorf("expected case label")
				}
				dst, nj, err := parseValueAt(rest, nj+len("label "))
				if err != nil {
					return nil, err
				}
				in.Cases = append(in.Cases, Case{Val: cv, Dest: dst.Name})
				_ = ty2
				ni = nj
			}
		}
	case "unreachable":
	case "call":
		return parseCall(rest, in)
	default:
		return nil, fmt.Errorf("unsupported instruction %q", in.Op)
	}
	return in, nil
}

func skipIRFlags(s string) string {
	for {
		switch {
		case strings.HasPrefix(s, "nsw "):
			s = strings.TrimSpace(s[4:])
		case strings.HasPrefix(s, "nuw "):
			s = strings.TrimSpace(s[4:])
		case strings.HasPrefix(s, "exact "):
			s = strings.TrimSpace(s[6:])
		case strings.HasPrefix(s, "inbounds "):
			s = strings.TrimSpace(s[9:])
		case strings.HasPrefix(s, "nneg "):
			s = strings.TrimSpace(s[5:])
		default:
			return s
		}
	}
}

func parseWordAt(s string, i int) (string, int, error) {
	i = skipSpace(s, i)
	j := i
	for j < len(s) && !isDelim(s[j]) {
		j++
	}
	if j == i {
		return "", i, fmt.Errorf("expected word at %q", s[i:])
	}
	return s[i:j], j, nil
}

func parseCall(rest string, in *Inst) (*Inst, error) {
	// Find the callee: the last '@' or '%' before '(' at top level.
	paren := strings.IndexByte(rest, '(')
	if paren < 0 {
		return nil, fmt.Errorf("call without arguments")
	}
	head := strings.TrimSpace(rest[:paren])
	// The callee is the token immediately before '('.
	at := strings.LastIndexAny(head, "@%")
	if at < 0 {
		return nil, fmt.Errorf("call without callee")
	}
	in.Callee = strings.TrimSpace(head[at+1:])
	in.Indirect = head[at] == '%'
	// Return type: the type token immediately before the callee, possibly
	// followed by attributes. Scan backwards for a type-looking token.
	rhead := strings.TrimSpace(head[:at])
	for {
		toks := strings.Fields(rhead)
		if len(toks) == 0 {
			in.Ty = &Type{Kind: KVoid}
			break
		}
		// Try the last token as a type; if it is an attribute, drop it.
		if t, _, err := parseTypeAt(toks[len(toks)-1], 0); err == nil {
			in.Ty = t
			break
		}
		rhead = strings.Join(toks[:len(toks)-1], " ")
	}
	rp := matchParen(rest, paren)
	if rp < 0 {
		return nil, fmt.Errorf("unterminated call")
	}
	args := strings.TrimSpace(rest[paren+1 : rp])
	if args != "" {
		for _, a := range splitTop(args) {
			if a == "..." {
				continue
			}
			ty, ni, err := parseTypeAt(a, 0)
			if err != nil {
				return nil, fmt.Errorf("call arg %q: %v", a, err)
			}
			v, _, err := parseValueAt(a, ni)
			if err != nil {
				return nil, fmt.Errorf("call arg %q: %v", a, err)
			}
			in.Ops = append(in.Ops, v)
			in.OpTys = append(in.OpTys, ty)
		}
	}
	return in, nil
}
