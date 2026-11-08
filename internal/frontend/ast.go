package frontend

// AST node definitions for the tiny C subset understood by goc.

type Node interface{}

// CType is the type of a value. Everything is 8 bytes on the stack; the only
// distinction is whether an expression lives in a GP register (int) or an XMM
// register (double) and how goclib / the ABI treat it.
type CType int

const (
	TInt CType = iota
	TDouble
	// TF128 is long double: IEEE binary128, 16 bytes. It is neither of the
	// two 8-byte slots above -- codegen carries it as a 16-byte aggregate
	// (goc) or as LLVM's fp128 (gocl), and every arithmetic operation on it
	// lowers to a soft-float helper. See frontend/fp128.go.
	TF128
)

type Program struct {
	Funcs []*FuncDecl // function definitions (Body != nil)
	// Prototypes are forward declarations from #include'd headers (Body ==
	// nil). They are used only by the type checker to validate calls; they
	// are never emitted as code, and they are deliberately kept out of the
	// code generator's "defined functions" set so that a prototype for a goclib
	// function (e.g. strlen) still triggers goclib inclusion rather than being
	// mistaken for a user definition.
	Prototypes []*FuncDecl
	// Globals are top-level variable declarations (with optional initialisers).
	// They are emitted into the .data section by the code generator and are
	// visible to every function.
	Globals []*DeclStmt
}

type FuncDecl struct {
	Name       string
	Ret        *Type
	Params     []string
	ParamTypes []*Type
	Variadic   bool
	Body       *Block
	// Storage is the storage-class specifier that preceded the definition
	// ("static", "extern" or ""), recorded since 2026-10-04 so a multi-file
	// build can give a static function internal linkage (one file's static
	// helper must not collide with another file's identically named one).
	Storage string
	// DLL is the import library named on a prototype: "extern long
	// MessageBoxA(...), user32;" -- the same `extern Name, dll` shape the
	// assembler takes. Empty means the symbol resolves to a goclib C function
	// (compiled in) or, on Linux, a syscall stub -- not a Windows DLL import.
	DLL string
}

type Block struct {
	Stmts []Stmt
}

// DeclList groups several comma-separated declarations from one statement.
type DeclList struct {
	Decls []*DeclStmt
}

type Stmt interface{}

type DeclStmt struct {
	Name string
	Typ  *Type
	Init Expr // may be nil
	// Storage is the storage-class specifier on the declaration: "", "static",
	// "extern", "register", "auto", or "typedef" (typedef is handled
	// separately and never reaches codegen). goc is a non-optimising,
	// single-translation-unit compiler, so register/auto are accepted as no-ops
	// and extern/static only matter for the static-local lifetime rule.
	Storage string
	// IsTLS is true when the declaration carried _Thread_local / thread_local:
	// the variable lives in thread-local storage and each thread gets its own
	// instance (accessed via the FS/GS segment on x86-64).
	IsTLS bool
	Line  int
}

type AssignStmt struct {
	Lhs Expr // must be an lvalue: Ident, Unary("*"), or Index
	Rhs Expr
}

// AssignExpr is an assignment used in expression position: "(a = b)" yields the
// value of b. It is produced by the parser wherever an assignment appears inside
// a larger expression (e.g. "while ((*d++ = *src++) != 0)"). Statement-level
// "a = b;" is likewise wrapped as ExprStmt{AssignExpr} so both share one
// code path.
type AssignExpr struct {
	Lhs Expr // must be an lvalue
	// Op is the compound-assignment operator in its binary spelling ("+", "-",
	// "*", "/", "%", "&", "|", "^", "<<", ">>"); it is "" for plain "=". The
	// parser no longer desugars "E1 op= E2" into "E1 = E1 op E2": that
	// evaluated E1's address twice ("a[i++] += 10" incremented i twice). The
	// checker and codegen treat Op != "" as "evaluate the lvalue once".
	Op  string
	Rhs Expr
}

// TmpLoad is an internal codegen-only node: it loads the value that a
// compound-assignment implementation parked in a frame temporary at Slot.
// The parser never produces it; genCompoundAssign builds it so genBinary's
// arithmetic (width promotion, float, pointer scaling) is reused verbatim
// without re-evaluating the lvalue.
type TmpLoad struct {
	Slot int
	Typ  *Type
}

// CommaExpr is the comma operator a, b, c: each operand is evaluated in turn
// (left to right) and the value of the whole expression is that of the
// rightmost operand. It is the lowest-precedence operator in C, below even
// assignment, so it only appears where a full expression is expected (statement
// expressions, the heads/tails of for-loops, the middle of a ternary).
type CommaExpr struct {
	Left  Expr
	Right Expr
}

type ExprStmt struct {
	E Expr
}

type ReturnStmt struct {
	E Expr // may be nil
}

type IfStmt struct {
	Cond Expr
	Then Stmt
	Else Stmt // may be nil
}

type WhileStmt struct {
	Cond Expr
	Body Stmt
}

// ForStmt is a C for-loop: for (Init; Cond; Post) Body. Init is a Stmt
// (typically a DeclStmt or ExprStmt); Cond and Post are expressions (either may
// be nil, meaning "absent").
type ForStmt struct {
	Init Stmt
	Cond Expr
	Post Expr
	Body Stmt
}

// DoWhileStmt is a C do-while loop: "do Body while (Cond);". The body always
// runs at least once, and the condition is tested after it -- so continue jumps
// to the condition rather than to the top of the body.
type DoWhileStmt struct {
	Body Stmt
	Cond Expr
}

// SwitchStmt is a C switch. The body is kept flat: CaseStmt and DefaultStmt are
// ordinary statements inside the block, exactly where the source puts them.
// That preserves C's fall-through semantics for free -- control dropping out of
// one case simply runs whatever statement follows it -- and it lets "default"
// sit anywhere in the body, not just at the end.
type SwitchStmt struct {
	Src  Expr
	Body *Block
	Line int
}

// CaseStmt is a "case N:" label. The value is folded to an integer constant by
// the parser (C requires a constant expression here), so codegen can compare
// against an immediate.
type CaseStmt struct {
	Val  int
	Line int
}

// DefaultStmt is a "default:" label.
type DefaultStmt struct {
	Line int
}

// GotoStmt is "goto label;". Labels are function-scoped; the checker verifies
// that the target exists in the same function.
type GotoStmt struct {
	Label string
	Line  int
}

// LabelStmt is "label: statement". Labelling a statement (rather than making a
// label its own statement) keeps the parse tree close to the C grammar and lets
// "x: ;" label an empty statement.
type LabelStmt struct {
	Name string
	Stmt Stmt
	Line int
}

// AsmStmt is an inline-assembly block "__asm { ... }". Text is the raw source
// between the braces, exactly as written (whitespace and comments included);
// the code generator binds C variable names in it to their frame slots or
// registers and hands it to goa verbatim.
type AsmStmt struct {
	Text string
	Line int
}

type BreakStmt struct{}

type ContinueStmt struct{}

type Expr interface{}

type NumLit struct {
	Val     int64
	Kind    CType
	Fval    float64
	IsFloat bool // a "1.5f" literal: type float rather than double
	// A "1.5L" literal: type long double (binary128). F128 holds the exact
	// 128-bit encoding; Fval is left at the float64 rounding for diagnostics
	// and is never what codegen materialises.
	IsLongDouble bool
	F128         Float128
	Unsig        bool // u/U suffix: the constant's type is unsigned
	Long         bool // l/L suffix: the constant is at least 64 bits wide
	// LongLong marks a two-l suffix ("1LL"): type long long, distinct from
	// long (C11 6.2.5p4) for _Generic purposes even at equal width.
	LongLong bool
	Wide     bool // L'x' wide character literal: type is wchar_t, not int
	// C23 bit-precise literal (wb/uwb suffix): BigWords holds the value as
	// little-endian 64-bit words and BigBits its declared _BitInt width.
	// Non-nil BigWords switches the literal's type to _BitInt(BigBits).
	BigWords  []uint64
	BigSigned bool
	BigBits   int
}

type StrLit struct {
	Bytes []byte
	// Wide marks an L"..." literal: Bytes holds UTF-16LE code units rather
	// than UTF-8 bytes and the literal's type is wchar_t*, not char*. See the
	// matching field on the lexer token for where the re-encode happens.
	Wide bool
}

// WCharType is the type of wchar_t: a 2-byte unsigned integer, matching
// Windows' UTF-16 code unit (and the typedef in <stddef.h>).
func WCharType() *Type { return &Type{Kind: KInt, Width: 2, Signed: false} }

// InitElem is one element of a brace initialiser. Desig holds the member
// name for a designated initialiser (".x = 1"); DesigIdx holds the array
// index for an array designator ("[2] = 1"). Exactly one of them is set for a
// designated element (Desig for struct/union members, DesigIdx for array
// elements); both are empty/negative for positional initialisation. E is the
// value, which may itself be a *BraceInit for a nested aggregate.
type InitElem struct {
	Desig    string
	DesigIdx int // array designator index ("[i] ="); -1 means "not a designator"
	E        Expr
}

// BraceInit is a braced initialiser "{ ... }" for an array, struct, union or
// (C allows it) a scalar. It only appears in the initialiser position of a
// declaration, never inside a general expression.
type BraceInit struct {
	Elems []InitElem
	Line  int
}

type Ident struct {
	Name string
	Line int
}

type Unary struct {
	Op string // "-" or "!"
	E  Expr
}

type Binary struct {
	Op string // "+" "-" "*" "/" "%" "<" ">" "<=" ">=" "==" "!=" "&&" "||" "&" "|" "^" "<<" ">>"
	L  Expr
	R  Expr
}

type Call struct {
	Name string
	Args []Expr
}

// IndirectCall is a call whose callee is computed at run time rather than
// named: (*fp)(x), tab[i](x), s.cb(x). Fn evaluates to the address of the
// function. A call written directly against an identifier is still a Call --
// even when that identifier turns out to be a function-pointer variable, which
// the type checker and code generator resolve later.
//
// A member call "x.f(args)" whose member f does not exist can still be valid:
// when the checker resolves it as a method on x's struct type (Go/Nim style
// UFCS, spelled T_f), it records the rewritten direct call here. Codegen then
// emits T_f with the receiver prepended to the arguments and ignores Fn.
type IndirectCall struct {
	Fn   Expr
	Args []Expr
	UFCS *Call
}

// Index is the subscript operator: arr[i] or ptr[i]. The base may be an array
// or a pointer; the element type is the base's element type.
type Index struct {
	Base Expr
	Idx  Expr
}

// CondExpr is the ternary operator a ? b : c.
type CondExpr struct {
	Cond Expr
	Then Expr
	Else Expr
}

// CastExpr is a C-style cast (Type)E. Value-level conversion is handled by the
// code generator (most integer widths share the 8-byte slot); only int<->double
// and width truncation/extension actually move bits.
type CastExpr struct {
	Typ *Type
	E   Expr
}

// IncDecExpr is a prefix (++x) or postfix (x++) increment/decrement. Prefix is
// true for ++x / --x, false for x++ / x--.
type IncDecExpr struct {
	Op     string // "++" or "--"
	E      Expr
	Prefix bool
}

// VaArgExpr is the va_arg(ap, Type) builtin of <stdarg.h>. The second "argument"
// is a type, not an expression, so the parser builds this dedicated node instead
// of a Call. Ap is the va_list cursor; Typ is the requested element type.
type VaArgExpr struct {
	Ap  Expr
	Typ *Type
}

// MemberExpr is the field access operator: "base.member" (. form) when Arrow is
// false, or "base->member" (-> form) when Arrow is true. In the -> form the base
// must have pointer-to-struct type and the compiler dereferences it before
// adding the member offset; in the . form the base must be a struct lvalue.
type MemberExpr struct {
	Base  Expr
	Name  string
	Arrow bool
	Line  int
}

// SizeofExpr is the sizeof operator: either "sizeof(Type)" or "sizeof expr". It
// is folded to a constant by the parser/checker where possible and lowered to a
// plain integer literal by the code generator.
type SizeofExpr struct {
	Typ *Type // non-nil when the operand is a type name
	E   Expr  // non-nil when the operand is an expression
}

// CompoundLit is the C99 compound literal "(T){...}": an unnamed object of
// type T, initialised from the brace list. It is an lvalue (its address can
// be taken) and each evaluation re-initialises the object. The checker
// validates the initialiser against Typ (filling an incomplete array's
// length) and codegen gives each occurrence a persistent frame slot.
type CompoundLit struct {
	Typ  *Type
	Init *BraceInit
	Line int
}

// GenericAssoc is one association of a _Generic selection: either
// "TypeName : expr" or the catch-all "default : expr". Exactly one of
// IsDefault / Typ is meaningful.
type GenericAssoc struct {
	Typ       *Type // the associated type (nil for default)
	IsDefault bool
	E         Expr
}

// GenericExpr is the C11/C23 generic selection "_Generic(ctrl, T1: e1, ...)".
// The controlling expression is never evaluated; the type checker computes its
// type (after lvalue conversion: decay + qualifier drop), picks the matching
// association -- or the default -- and records it in Chosen/ChosenIdx. Only
// Chosen is type-checked and emitted; the other branches are syntax only.
type GenericExpr struct {
	Control   Expr
	Assocs    []GenericAssoc
	Chosen    Expr // set by the checker; codegen emits only this
	ChosenIdx int
	Line      int
}
