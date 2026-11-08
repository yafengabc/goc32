package frontend

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

type TokKind int

const (
	TEOF TokKind = iota
	TIdent
	TNum
	TStr
	TPunct
	TKeyword
	// TAsm carries the raw source text of a "__asm { ... }" block (the bytes
	// between the braces, exactly as written). The block is handed through to
	// the assembler verbatim, so the lexer captures the original spelling
	// rather than a token stream -- whitespace, indentation and comment
	// characters are all significant to goa.
	TAsm
)

var keywords = map[string]bool{
	"int": true, "double": true, "float": true, "if": true, "else": true, "while": true, "return": true,
	"char": true, "long": true, "short": true, "unsigned": true, "signed": true,
	"void": true, "struct": true, "union": true, "enum": true, "_Bool": true, "bool": true, "true": true, "false": true,
	"for": true, "break": true, "continue": true,
	"switch": true, "case": true, "default": true, "do": true, "goto": true,
	"typedef": true, "extern": true, "static": true,
	"register": true, "auto": true,
	"const": true, "volatile": true, "restrict": true,
	"static_assert": true, "_Static_assert": true,
	"alignas": true, "alignof": true,
	"noreturn": true, "_Noreturn": true,
	"_Generic":     true,
	"_BitInt":      true,
	"char8_t":      true,
	"inline":       true,
	"thread_local": true, "_Thread_local": true,
	// __thread is the GNU spelling of C11 _Thread_local. Real code uses it:
	// Nim's nimbase.h defines NIM_THREADVAR to __thread for the MinGW/gcc
	// target, so every Nim-generated translation unit carries it.
	"__thread":      true,
	"__has_include": true, "__has_c_attribute": true,
	"__VA_OPT__": true,
	"_Atomic":    true,
}

type Token struct {
	Kind    TokKind
	Text    string
	Num     int64
	Fval    float64
	IsDbl   bool
	IsFloat bool // a float constant: a floating literal with the f/F suffix
	// A floating literal with the l/L suffix: its type is long double. F128
	// holds the exact binary128 encoding; Fval keeps the float64 rounding for
	// diagnostics only and must never be what codegen materialises, because
	// a long double constant can carry 113 bits of mantissa.
	IsLongDouble bool
	F128         Float128
	IsChar       bool // a 'x' character literal (carried as an integer constant in Num)
	IsUnsig      bool // integer literal with a u/U suffix: type is unsigned
	IsLong       bool // integer literal with an l/L suffix: at least 64 bits wide
	// IsLL marks a two-l suffix ("1LL"): the constant's type is long long,
	// which C keeps distinct from long (6.2.5p4) -- observable through
	// _Generic even though both are 64 bits here.
	IsLL bool
	// C23 bit-precise integer literal (wb/uwb suffix): the value parsed as an
	// arbitrary-precision big integer, split into little-endian 64-bit words.
	BigWords  []uint64
	BigSigned bool
	BigBits   int // declared bit width of the literal's own _BitInt type
	Str       []byte
	// Wide marks an L"..." literal: Str holds UTF-16LE code units (2 bytes
	// each) instead of UTF-8 bytes, and the literal's type is wchar_t* rather
	// than char*. The terminating NUL is added by the code generator, which
	// knows whether it must be 1 or 2 bytes wide.
	Wide  bool
	Line  int
	Space bool // true if whitespace preceded this token (separates macro name from '(' etc.)
}

// pushBig builds the token for a C23 bit-precise integer literal: the value's
// 64-bit words plus its own declared _BitInt type (the smallest signed or
// unsigned width that holds the value, per C23 6.7.3).
func pushBig(push func(Token), words []uint64, bitLen int, unsig bool, line int) {
	n := bitLen + 1
	if unsig {
		n = bitLen
	}
	if n < 1 {
		n = 1
	}
	if n > 4096 {
		n = 4096
	}
	w := words
	if bitLen <= 64 && len(w) > 1 {
		w = w[:1]
	}
	push(Token{Kind: TNum, Text: "wb", BigWords: w, BigSigned: !unsig, BigBits: n, Line: line})
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// floatSuffix scans the suffix of a floating literal: f/F narrows it to float,
// l/L makes it long double. (The same letters mean "at least 64 bits wide" on
// an *integer* literal, which is why this only ever runs on the float paths --
// isDbl has already been decided by the time it is called.) ni is the position
// after the suffix.
func floatSuffix(src string, i, n int) (isFloat, isLongDbl bool, ni int) {
	for i < n && (src[i] == 'f' || src[i] == 'F' || src[i] == 'l' || src[i] == 'L') {
		if src[i] == 'f' || src[i] == 'F' {
			isFloat = true
		} else {
			isLongDbl = true
		}
		i++
	}
	return isFloat, isLongDbl, i
}

// floatToken builds the token for a floating literal, converting a long double
// constant straight to its binary128 encoding.
func floatToken(text string, line int, isFloat, isLongDbl bool) Token {
	f, _ := strconv.ParseFloat(text, 64)
	tok := Token{Kind: TNum, Text: text, Fval: f, IsDbl: true,
		IsFloat: isFloat, IsLongDouble: isLongDbl, Line: line}
	if isLongDbl {
		// A malformed constant cannot reach here -- the scanner only gets
		// this far on well-formed digits -- so a conversion failure is a bug
		// rather than a user error; falling back to zero keeps the token
		// usable instead of dropping it.
		if v, err := ParseFloat128(text); err == nil {
			tok.F128 = v
		}
	}
	return tok
}

// bigSuffix scans an optional C23 bit-precise integer suffix ("wb" / "uwb",
// case-insensitive) at src[i:n]. It reports whether the suffix is present
// and whether it carried the u (unsigned) flag; ni is the position after it.
func bigSuffix(src string, i, n int) (isBig, unsig bool, ni int) {
	j := i
	u := false
	if j < n && (src[j] == 'u' || src[j] == 'U') {
		u = true
		j++
	}
	if j+1 < n && (src[j] == 'w' || src[j] == 'W') && (src[j+1] == 'b' || src[j+1] == 'B') {
		return true, u, j + 2
	}
	return false, false, i
}

// parseBigLiteral parses an integer literal's digit text (decimal, 0x hex or
// 0b binary) as an arbitrary-precision value and splits it into little-endian
// 64-bit words. The declared _BitInt type follows the C23 rule: the smallest
// signed/unsigned bit-precise type that holds the value.
func parseBigLiteral(text string) ([]uint64, int, error) {
	v, ok := new(big.Int).SetString(text, 0)
	if !ok {
		return nil, 0, fmt.Errorf("invalid bit-precise integer literal %q", text)
	}
	if v.Sign() < 0 {
		return nil, 0, fmt.Errorf("bit-precise integer literal must be non-negative")
	}
	// Pack big-endian bytes into little-endian 64-bit words (independent of
	// big.Word's platform width).
	bs := v.Bytes()
	words := make([]uint64, (len(bs)+7)/8)
	for k, b := range bs {
		// k counts from the most significant byte; its weight from the
		// bottom of the number is (len-1-k)*8 bits.
		bitPos := uint((len(bs) - 1 - k) * 8)
		words[bitPos/64] |= uint64(b) << (bitPos % 64)
	}
	return words, v.BitLen(), nil
}
func isAlpha(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// hexVal returns the numeric value (0-15) of a hexadecimal digit, or -1 if b
// is not a hex digit.
func hexVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10
	}
	return -1
}

// isHexDigit reports whether b is a hexadecimal digit.
func isHexDigit(b byte) bool { return hexVal(b) >= 0 }

// decodeEscape decodes one C escape sequence. i points at the first source
// character *after* the backslash. It appends the decoded byte(s) to buf and
// returns the buffer, the offset just past the escape, and any error. It
// covers the simple escapes, octal \ooo (1-3 digits), hex \xhh (greedy), and
// universal character names \uXXXX / \UXXXXXXXX (encoded as UTF-8), per C89
// 6.1.3.4 and C99 6.4.3 / 6.4.4.4.
// utf16le re-encodes a UTF-8 string literal payload as UTF-16LE code units,
// which is what a wchar_t array holds on Windows (2-byte wchar_t). Surrogate
// pairs are emitted for code points above the BMP. The result carries no
// terminator: the code generator appends the NUL, since only it knows whether
// the target slot is 1 or 2 bytes wide.
func utf16le(s []byte) []byte {
	out := make([]byte, 0, 2*(len(s)+1))
	for _, r := range string(s) {
		switch {
		case r < 0x10000:
			out = append(out, byte(r), byte(r>>8))
		default:
			r -= 0x10000
			hi := 0xD800 + (r >> 10)
			lo := 0xDC00 + (r & 0x3FF)
			out = append(out, byte(hi), byte(hi>>8), byte(lo), byte(lo>>8))
		}
	}
	return out
}

func decodeEscape(buf []byte, src string, i, line int) ([]byte, int, error) {
	n := len(src)
	c := src[i]
	switch c {
	case 'n':
		buf = append(buf, '\n')
		i++
	case 't':
		buf = append(buf, '\t')
		i++
	case 'r':
		buf = append(buf, '\r')
		i++
	case 'b':
		buf = append(buf, '\b')
		i++
	case 'f':
		buf = append(buf, '\f')
		i++
	case 'a':
		buf = append(buf, '\a')
		i++
	case 'v':
		buf = append(buf, '\v')
		i++
	case '\\':
		buf = append(buf, '\\')
		i++
	case '\'':
		buf = append(buf, '\'')
		i++
	case '"':
		buf = append(buf, '"')
		i++
	case '?':
		buf = append(buf, '?')
		i++
	case '0', '1', '2', '3', '4', '5', '6', '7':
		v := int(c - '0')
		i++
		for k := 0; k < 2 && i < n && src[i] >= '0' && src[i] <= '7'; k++ {
			v = v*8 + int(src[i]-'0')
			i++
		}
		buf = append(buf, byte(v))
	case 'x', 'X':
		i++
		if i >= n || !isHexDigit(src[i]) {
			return buf, i, fmt.Errorf("line %d: '\\x' used with no following hex digits", line)
		}
		v := 0
		for i < n && isHexDigit(src[i]) {
			v = v*16 + hexVal(src[i])
			i++
		}
		buf = append(buf, byte(v))
	case 'u', 'U':
		need := 4
		if c == 'U' {
			need = 8
		}
		i++
		var cv uint32
		for k := 0; k < need; k++ {
			if i >= n || !isHexDigit(src[i]) {
				return buf, i, fmt.Errorf("line %d: incomplete universal character name", line)
			}
			cv = cv*16 + uint32(hexVal(src[i]))
			i++
		}
		if (cv >= 0xD800 && cv <= 0xDFFF) || cv > 0x10FFFF {
			return buf, i, fmt.Errorf("line %d: universal character name \\u%X does not denote a character", line, cv)
		}
		buf = append(buf, []byte(string(rune(cv)))...)
	default:
		buf = append(buf, c)
		i++
	}
	return buf, i, nil
}

// Lex turns C source into a token slice.
//
// The preprocessor runs on the token stream this produces, so the '#' that
// begins a directive is emitted as a normal punctuation token rather than
// dropped: the preprocessor recognises a directive as a '#' that opens a new
// line. Token.Space records whether whitespace preceded the token, which the
// preprocessor needs to tell a function-like macro "F(x)" from an object-like
// macro "F (x)" (the space between name and '(' is significant in C).
func Lex(src string) ([]Token, error) {
	var toks []Token
	line := 1
	space := false
	push := func(t Token) {
		t.Space = space
		toks = append(toks, t)
		space = false
	}
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		// Wide literal prefix: L"..." / L'...' (C11 6.4.5). The payload is
		// UTF-16LE, matching goc's 2-byte wchar_t, so the prefix is NOT a
		// no-op like u8 above -- it changes both the encoding and the type.
		// 'L' alone is still an ordinary identifier (LONG, Label, ...), so
		// only the prefix-adjacent forms are consumed here.
		wide := false
		if c == 'L' && i+1 < n && (src[i+1] == '"' || src[i+1] == '\'') {
			i++
			c = src[i]
			wide = true
		}
		// C23 u8 string/char prefix: u8"..." / u8'...'. goc strings are already
		// UTF-8, so the prefix is a no-op semantically; consume "u8" and let the
		// normal string/char literal lexing below handle the rest.
		if c == 'u' && i+2 < n && src[i+1] == '8' && (src[i+2] == '"' || src[i+2] == '\'') {
			i += 2
			c = src[i]
		}
		switch {
		case c == '\n':
			line++
			space = true
			i++
		case c == ' ' || c == '\t' || c == '\r':
			space = true
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			space = true
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '#':
			// '##' is the token-paste operator and must be a single token; a
			// lone '#' begins a directive or is the stringisation operator.
			if i+1 < n && src[i+1] == '#' {
				push(Token{Kind: TPunct, Text: "##", Line: line})
				i += 2
			} else {
				push(Token{Kind: TPunct, Text: "#", Line: line})
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i < n && !(src[i] == '*' && i+1 < n && src[i+1] == '/') {
				if src[i] == '\n' {
					line++
				}
				i++
			}
			i += 2
		case isDigit(c):
			start := i
			isDbl := false
			// Hex literal: 0x[0-9a-fA-F]+. The size suffixes are recorded
			// rather than dropped: 1LL has to *be* 64 bits wide, because a
			// 32-bit `1` shifted left by 52 is zero while `1LL << 52` is not.
			if c == '0' && i+1 < n && (src[i+1] == 'x' || src[i+1] == 'X') {
				i += 2
				for i < n && (isDigit(src[i]) ||
					(src[i] >= 'a' && src[i] <= 'f') ||
					(src[i] >= 'A' && src[i] <= 'F') || src[i] == 39) {
					i++
				}
				// C23 bit-precise suffix: 0xFFuwb / 0x1Abcwb. Checked before the
				// float forms because 'w' can never continue a hex number.
				if isW, u, ni := bigSuffix(src, i, n); isW {
					words, bitLen, err := parseBigLiteral(strings.ReplaceAll(src[start:i], "'", ""))
					if err != nil {
						return nil, fmt.Errorf("line %d: %s", line, err.Error())
					}
					pushBig(push, words, bitLen, u, line)
					i = ni
					continue
				}
				// Hexadecimal floating literal (C99/C23): 0x1.8p3, 0x.8p1,
				// 0x1p-2. The binary-exponent part (p/P[+-]digits) is optional
				// since C23, so "0x1.8" is also a valid double. 'p' can never
				// start an integer continuation -- it is not a hex digit -- so
				// 0x1e5 (e IS a hex digit) stays an integer and only '.'/'p'
				// switch to the float path.
				isHexDbl := false
				if i < n && src[i] == '.' {
					isHexDbl = true
					i++
					for i < n && ((src[i] >= 'a' && src[i] <= 'f') ||
						(src[i] >= 'A' && src[i] <= 'F') || isDigit(src[i]) || src[i] == 39) {
						i++
					}
				}
				if i < n && (src[i] == 'p' || src[i] == 'P') {
					j := i + 1
					if j < n && (src[j] == '+' || src[j] == '-') {
						j++
					}
					if j < n && isDigit(src[j]) {
						isHexDbl = true
						i = j
						for i < n && isDigit(src[i]) {
							i++
						}
					}
				}
				raw := src[start:i]
				text := strings.ReplaceAll(raw, "'", "")
				if isHexDbl {
					// f/F/l/L suffixes follow the decimal-float convention:
					// f/F narrows to float, l/L makes it long double (binary128).
					isFloat, isLongDbl, ni := floatSuffix(src, i, n)
					i = ni
					// Go's ParseFloat only accepts hex floats with a binary
					// exponent; C23 allows its omission, so add a p0 for it.
					if !strings.ContainsAny(text, "pP") {
						text += "p0"
					}
					push(floatToken(text, line, isFloat, isLongDbl))
					continue
				}
				// Parse as an unsigned bit pattern so that 0x8000000000000000
				// (2^63) survives as the int64 minimum: -0x8000000000000000 is
				// the standard spelling of LONG_MIN. ParseInt would overflow
				// on it and leave v == 0.
				u, err := strconv.ParseUint(text[2:], 16, 64)
				var v int64
				if err == nil {
					v = int64(u)
				}
				unsig, long, ll := false, false, false
				for i < n && (src[i] == 'u' || src[i] == 'U' || src[i] == 'l' || src[i] == 'L') {
					if src[i] == 'u' || src[i] == 'U' {
						unsig = true
					} else if long {
						ll = true // a second l/L suffix: "1LL" is long long
					} else {
						long = true
					}
					i++
				}
				push(Token{Kind: TNum, Text: text, Num: v, IsUnsig: unsig, IsLong: long, IsLL: ll, Line: line})
				continue
			}
			// Binary literal: 0b[01]+ (C23), optional ' separators.
			if c == '0' && i+1 < n && (src[i+1] == 'b' || src[i+1] == 'B') {
				i += 2
				for i < n && (src[i] == '0' || src[i] == '1' || src[i] == 39) {
					i++
				}
				raw := src[start:i]
				btext := strings.ReplaceAll(raw, "'", "")
				u, err := strconv.ParseUint(btext[2:], 2, 64)
				var v int64
				if err == nil {
					v = int64(u)
				}
				unsig, long, ll := false, false, false
				for i < n && (src[i] == 'u' || src[i] == 'U' || src[i] == 'l' || src[i] == 'L') {
					if src[i] == 'u' || src[i] == 'U' {
						unsig = true
					} else if long {
						ll = true // a second l/L suffix: "1LL" is long long
					} else {
						long = true
					}
					i++
				}
				push(Token{Kind: TNum, Text: raw, Num: v, IsUnsig: unsig, IsLong: long, IsLL: ll, Line: line})
				continue
			}
			for i < n && (isDigit(src[i]) || src[i] == 39) {
				i++
			}
			if i < n && src[i] == '.' {
				isDbl = true
				i++
				for i < n && (isDigit(src[i]) || src[i] == 39) {
					i++
				}
			}
			// Exponent: [eE][+-]?digits. Only taken when at least one digit
			// follows, so "1e" or "1ex" still lex as an integer plus an
			// identifier.
			if i < n && (src[i] == 'e' || src[i] == 'E') {
				j := i + 1
				if j < n && (src[j] == '+' || src[j] == '-') {
					j++
				}
				if j < n && isDigit(src[j]) {
					isDbl = true
					i = j
					for i < n && (isDigit(src[i]) || src[i] == 39) {
						i++
					}
				}
			}
			raw := src[start:i]
			text := strings.ReplaceAll(raw, "'", "")
			// C23 bit-precise suffix after the decimal digits (42wb, 42uwb),
			// before the u/l suffix loop: bigSuffix owns the optional u.
			if !isDbl {
				if isW, u, ni := bigSuffix(src, i, n); isW {
					words, bitLen, err := parseBigLiteral(text)
					if err != nil {
						return nil, fmt.Errorf("line %d: %s", line, err.Error())
					}
					pushBig(push, words, bitLen, u, line)
					i = ni
					continue
				}
			}
			if isDbl {
				isFloat, isLongDbl, ni := floatSuffix(src, i, n)
				i = ni
				push(floatToken(text, line, isFloat, isLongDbl))
			} else {
				// Parse as an unsigned bit pattern: 9223372036854775808 (2^63)
				// maps to the int64 minimum, so -9223372036854775808L works.
				// Sscanf's %d overflows on it and leaves v == 0, which turns
				// LONG_MIN into -0 == 0. Values beyond 2^64-1 stay 0.
				// A leading '0' that is not 0x/0b (handled above) denotes an octal
				// constant (C89 6.1.3.1): 010 is 8, 0777 is 511. The octal
				// reading only applies to a pure integer -- a literal that later
				// shows a '.' or an exponent was already switched to the float path
				// above, where its digits are read as decimal. An 8 or 9 directly
				// after a leading zero is an invalid octal digit (gcc rejects).
				base := 10
				if len(text) > 0 && text[0] == '0' {
					bad := byte(0)
					octal := true
					for k := 1; k < len(text); k++ {
						if text[k] >= '8' {
							octal = false
							bad = text[k]
							break
						}
					}
					if octal {
						base = 8
					} else {
						return nil, fmt.Errorf("line %d: invalid digit '%c' in octal constant", line, bad)
					}
				}
				u, err := strconv.ParseUint(text, base, 64)
				var v int64
				if err == nil {
					v = int64(u)
				}
				unsig, long, ll := false, false, false
				for i < n && (src[i] == 'u' || src[i] == 'U' || src[i] == 'l' || src[i] == 'L') {
					if src[i] == 'u' || src[i] == 'U' {
						unsig = true
					} else if long {
						ll = true // a second l/L suffix: "1LL" is long long
					} else {
						long = true
					}
					i++
				}
				push(Token{Kind: TNum, Text: text, Num: v, IsUnsig: unsig, IsLong: long, IsLL: ll, Line: line})
			}
		case c == '.' && i+1 < n && isDigit(src[i+1]):
			// Leading-dot floating constant: .5, .5f, .5e2, .5L, .1'2 (C23).
			// Only taken when a digit directly follows the '.', so member access
			// "a.b", the "->" token, "..." and ternary are untouched.
			start := i
			i++ // consume leading '.'
			for i < n && (isDigit(src[i]) || src[i] == 39) {
				i++
			}
			// Exponent: [eE][+-]?digits, taken only when a digit follows.
			if i < n && (src[i] == 'e' || src[i] == 'E') {
				j := i + 1
				if j < n && (src[j] == '+' || src[j] == '-') {
					j++
				}
				if j < n && isDigit(src[j]) {
					i = j
					for i < n && (isDigit(src[i]) || src[i] == 39) {
						i++
					}
				}
			}
			raw := src[start:i]
			text := strings.ReplaceAll(raw, "'", "")
			isFloat, isLongDbl, ni := floatSuffix(src, i, n)
			i = ni
			push(floatToken(text, line, isFloat, isLongDbl))
		case isAlpha(c):
			start := i
			for i < n && (isAlpha(src[i]) || isDigit(src[i])) {
				i++
			}
			raw := src[start:i]
			text := strings.ReplaceAll(raw, "'", "")
			// Inline assembly: "__asm { ... }" (and the MSVC alias "_asm")
			// begins a block whose body is raw assembler text, not C. The
			// block is only entered when the keyword is directly followed by
			// '{' -- a bare "__asm" used as an ordinary identifier (or a GNU
			// asm("...") call) lexes normally.
			if (text == "__asm" || text == "_asm") && asmBraceFollows(src, i) {
				// The switch-case body is its own scope in Go, so a := here
				// would shadow the outer i/line and both fail to compile
				// ("declared and not used: i") and, worse, discard the line
				// counter the asm block advanced past -- making every later
				// token carry the wrong line number. Assign to the outer
				// variables explicitly instead.
				var asmText string
				var asmErr error
				asmText, i, line, asmErr = lexAsmBlock(src, i, line)
				if asmErr != nil {
					return nil, asmErr
				}
				push(Token{Kind: TKeyword, Text: text, Line: line})
				push(Token{Kind: TAsm, Text: asmText, Line: line})
				continue
			}
			if keywords[text] {
				push(Token{Kind: TKeyword, Text: text, Line: line})
			} else {
				push(Token{Kind: TIdent, Text: text, Line: line})
			}
		case c == '"':
			i++
			var buf []byte
			for i < n && src[i] != '"' {
				if src[i] == '\\' && i+1 < n {
					i++
					err := error(nil)
					buf, i, err = decodeEscape(buf, src, i, line)
					if err != nil {
						return nil, err
					}
				} else {
					buf = append(buf, src[i])
					i++
				}
			}
			if i >= n {
				return nil, fmt.Errorf("line %d: unterminated string literal", line)
			}
			i++ // closing quote
			if wide {
				buf = utf16le(buf)
			}
			push(Token{Kind: TStr, Str: buf, Line: line, Wide: wide})
		case c == '\'':
			// Character literal 'x' (or '\n', '\0', ...). Carried as an integer
			// constant (the byte value) so the parser/codegen need no new node.
			i++
			var ch byte
			if i < n && src[i] == '\\' && i+1 < n {
				i++
				err := error(nil)
				ebuf := []byte(nil)
				ebuf, i, err = decodeEscape(ebuf, src, i, line)
				if err != nil {
					return nil, err
				}
				if len(ebuf) > 0 {
					ch = ebuf[0]
				}
			} else if i < n {
				ch = src[i]
				i++
			}
			if i >= n || src[i] != '\'' {
				return nil, fmt.Errorf("line %d: unterminated character literal", line)
			}
			i++ // closing quote
			var cv int64
			if wide {
				// L'x' is a wchar_t constant: an unsigned value in the range of
				// wchar_t (0..0xFFFF here). No sign extension -- '\xff' is 255,
				// not -1, because the wide literal's type is unsigned short.
				cv = int64(ch)
			} else {
				// goc char is signed: a character constant's value is the byte
				// sign-extended to int (C89 6.1.3.4), so '\377' (0xFF) is -1,
				// matching gcc rather than the raw byte 255.
				cv = int64(ch)
				if cv >= 128 {
					cv -= 256
				}
			}
			push(Token{Kind: TNum, Text: string(rune(ch)), Num: cv, IsChar: true, Wide: wide, Line: line})
		default:
			two := ""
			if i+1 < n {
				two = src[i : i+2]
			}
			three := ""
			if i+2 < n {
				three = src[i : i+3]
			}
			switch three {
			case "<<=", ">>=", "...":
				push(Token{Kind: TPunct, Text: three, Line: line})
				i += 3
				continue
			}
			switch two {
			case "==", "!=", "<=", ">=", "&&", "||", "##", "<<", ">>", "++", "--",
				"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "->":
				push(Token{Kind: TPunct, Text: two, Line: line})
				i += 2
				continue
			}
			// '^' (xor) and '~' (bitwise not) complete the bitwise family the
			// language already had (& | << >>).
			if strings.IndexByte("+-*/%=<>!(){};,.[]&?:|^~", c) >= 0 {
				push(Token{Kind: TPunct, Text: string(c), Line: line})
				i++
				continue
			}
			return nil, fmt.Errorf("line %d: unexpected character %q", line, c)
		}
	}
	push(Token{Kind: TEOF, Line: line})
	return toks, nil
}

// asmBraceFollows reports whether the source at src[i:] (after whitespace)
// opens a '{' -- i.e. an "__asm" token is followed by an inline-assembly
// block rather than being an ordinary identifier.
func asmBraceFollows(src string, i int) bool {
	for i < len(src) {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return src[i] == '{'
		}
	}
	return false
}

// lexAsmBlock consumes the body of a "__asm { ... }" block whose opening '{'
// sits at src[i] and returns the raw text between the braces, the source
// offset just past the matching '}', and the updated line counter.
//
// The body is assembler text, so its own quoting rules apply: a ';' or "//"
// starts a line comment that runs to end of line, and string/char literals
// are copied verbatim. '{' and '}' are only significant outside strings and
// comments, and nesting depth is tracked so that stray braces (or a '}' in a
// comment) never end the block early.
func lexAsmBlock(src string, i, line int) (string, int, int, error) {
	n := len(src)
	// The caller passes the position just past the "__asm" keyword, which may
	// be followed by whitespace before the opening '{'. Skip it (counting
	// newlines so the line counter stays correct) and only then treat '{' as
	// the depth-1 brace -- otherwise the '{' is counted as a nested brace and
	// the first '}' drops depth to 1 instead of 0, swallowing everything up
	// to the enclosing block's '}'.
	for i < n && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n') {
		if src[i] == '\n' {
			line++
		}
		i++
	}
	if i < n && src[i] == '{' {
		i++
	}
	var buf []byte
	depth := 1
	var inStr byte
	for i < n {
		c := src[i]
		if inStr != 0 {
			buf = append(buf, c)
			if c == '\\' && i+1 < n {
				i++
				buf = append(buf, src[i])
				i++
				continue
			}
			if c == inStr {
				inStr = 0
			}
			i++
			continue
		}
		switch {
		case c == '"' || c == '\'':
			inStr = c
			buf = append(buf, c)
			i++
		case c == ';' || (c == '/' && i+1 < n && src[i+1] == '/'):
			// Line comment: copy to end of line verbatim, so a '}' inside
			// a comment is ignored by the brace matcher.
			for i < n && src[i] != '\n' {
				buf = append(buf, src[i])
				i++
			}
		case c == '\n':
			line++
			buf = append(buf, c)
			i++
		case c == '{':
			depth++
			buf = append(buf, c)
			i++
		case c == '}':
			depth--
			if depth == 0 {
				i++
				return string(buf), i, line, nil
			}
			buf = append(buf, c)
			i++
		default:
			buf = append(buf, c)
			i++
		}
	}
	return "", 0, line, fmt.Errorf("line %d: unterminated __asm block (missing '}')", line)
}
