package frontend

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// This file carries goc's long double. The choice made for it is binary128
// (IEEE-754 quad precision) implemented in software on every target -- x86-64,
// ARM and RISC-V alike -- rather than the platform's native long double (x87
// 80-bit on x86-64, binary128 on aarch64). One format everywhere is the whole
// point: a long double program has to produce the same bits on every backend,
// and the only way to get that is to not depend on what the hardware offers.
//
// Go has no float128, so a long double constant is parsed straight into its
// 128-bit encoding here. The decimal form is read as an exact rational and
// rounded exactly once (round-half-to-even) -- routing it through a float64
// first would round twice and produce a different answer for constants that
// fall near a binary128 rounding boundary.

// EnableLongDouble turns `long double` into the real binary128 type instead
// of the double it has always been folded into.
//
// FORMER SCAFFOLDING, now unconditional: #48 landed the native back end's
// fp128 codegen, so every back end emits the real type and the flag is true
// forever. It stays a named variable only so the reader can grep the three
// places (parser literal, parser type specifier, checker) that still consult
// it; the drivers' assignments remain as belt-and-braces.
//
// The value it guards is not small -- with it off, `long double` is a 53-bit
// double that happens to be spelled long double, and sizeof(long double) is 8.
var EnableLongDouble = true

// Float128 is an IEEE-754 binary128 value in raw word form: Hi holds bits
// 127..64 (sign, the 15-bit exponent and the top 48 mantissa bits) and Lo
// holds bits 63..0. Codegen materialises a long double constant from these two
// words; nothing ever passes through a Go float64.
type Float128 struct {
	Hi uint64
	Lo uint64
}

const (
	f128SignMask = uint64(1) << 63
	f128ExpBits  = 15
	f128ExpBias  = 16383  // unbiased exponent of 1.0
	f128ExpMax   = 0x7FFF // exponent field of inf/nan
	f128MantBits = 112    // stored mantissa bits (the leading 1 is implicit)
	f128MinExp   = -16382 // unbiased exponent of the smallest normal
	// Bit 111 is the top stored mantissa bit; it lands in Hi at this position
	// and is what makes a NaN quiet.
	f128QuietHi = uint64(1) << 47
)

// Bits returns the 128-bit encoding as a big-endian hex string, which is how
// the tests and diagnostics spell a long double constant.
func (f Float128) Bits() string {
	return fmt.Sprintf("%016x%016x", f.Hi, f.Lo)
}

// IsZero reports whether f is +0 or -0 (the sign bit is ignored).
func (f Float128) IsZero() bool { return (f.Hi&^f128SignMask) == 0 && f.Lo == 0 }

// IsInf reports whether f is an infinity of either sign.
func (f Float128) IsInf() bool {
	return (f.Hi>>48)&f128ExpMax == f128ExpMax && f.Lo == 0 && (f.Hi&((uint64(1)<<48)-1)) == 0
}

// ParseFloat128 converts a C floating literal -- decimal or hexadecimal, with
// an optional exponent and C23 digit separators -- into its binary128 encoding.
func ParseFloat128(text string) (Float128, error) {
	s := strings.TrimSpace(text)
	neg := false
	switch {
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	case strings.HasPrefix(s, "-"):
		neg = true
		s = s[1:]
	}
	s = strings.ReplaceAll(s, "'", "")
	if s == "" {
		return Float128{}, fmt.Errorf("empty floating constant")
	}
	var num, den *big.Int
	var err error
	if strings.HasPrefix(strings.ToLower(s), "0x") {
		num, den, err = hexRat(s)
	} else {
		num, den, err = decRat(s)
	}
	if err != nil {
		return Float128{}, err
	}
	return f128FromRat(num, den, neg), nil
}

// decRat returns the exact rational value of a decimal floating literal.
func decRat(s0 string) (*big.Int, *big.Int, error) {
	s := s0
	exp10 := 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		tail := strings.TrimPrefix(s[i+1:], "+")
		e, err := strconv.Atoi(tail)
		if err != nil {
			return nil, nil, fmt.Errorf("bad exponent in %q", s0)
		}
		exp10 = e
		s = s[:i]
	}
	intp, fracp := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intp, fracp = s[:i], s[i+1:]
	}
	if intp == "" {
		intp = "0"
	}
	digits := intp + fracp
	m, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, nil, fmt.Errorf("bad floating constant %q", s0)
	}
	num := new(big.Int).Set(m)
	den := big.NewInt(1)
	exp10 -= len(fracp)
	if exp10 >= 0 {
		num.Mul(num, powBig(10, exp10))
	} else {
		den = powBig(10, -exp10)
	}
	return num, den, nil
}

// hexRat returns the exact rational value of a C hexadecimal floating literal
// (0x1.8p3). Hex floats are exact by construction: the mantissa is an integer
// and the scale is a power of two, so no rounding happens here at all.
func hexRat(s0 string) (*big.Int, *big.Int, error) {
	body := s0[2:]
	exp2 := 0
	if i := strings.IndexAny(body, "pP"); i >= 0 {
		tail := strings.TrimPrefix(body[i+1:], "+")
		e, err := strconv.Atoi(tail)
		if err != nil {
			return nil, nil, fmt.Errorf("bad exponent in %q", s0)
		}
		exp2 = e
		body = body[:i]
	}
	intp, fracp := body, ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intp, fracp = body[:i], body[i+1:]
	}
	if intp == "" {
		intp = "0"
	}
	m, ok := new(big.Int).SetString(intp+fracp, 16)
	if !ok {
		return nil, nil, fmt.Errorf("bad hex floating constant %q", s0)
	}
	num := new(big.Int).Set(m)
	den := big.NewInt(1)
	exp2 -= 4 * len(fracp)
	if exp2 >= 0 {
		num.Lsh(num, uint(exp2))
	} else {
		den.Lsh(den, uint(-exp2))
	}
	return num, den, nil
}

func powBig(base, n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(int64(base)), big.NewInt(int64(n)), nil)
}

// f128FromRat rounds the exact rational num/den to the nearest binary128,
// ties to even. num/den is taken as a magnitude; neg carries the sign.
func f128FromRat(num, den *big.Int, neg bool) Float128 {
	var sign uint64
	if neg {
		sign = f128SignMask
	}
	if num.Sign() == 0 || den.Sign() == 0 {
		return Float128{Hi: sign}
	}
	if num.Sign() < 0 {
		num = new(big.Int).Neg(num)
	}
	if den.Sign() < 0 {
		den = new(big.Int).Neg(den)
	}
	// e = floor(log2(num/den)). Bit lengths put it within one of the answer:
	// num is in [2^(nb-1), 2^nb) and den in [2^(db-1), 2^db), so the quotient
	// lies in (2^(nb-db-1), 2^(nb-db+1)) -- one exact comparison settles it.
	e := num.BitLen() - den.BitLen()
	if cmpPow2(num, den, e) < 0 {
		e--
	}
	// Subnormals are handled by rounding on the clamped grid: a value that
	// rounds up to 2^112 then comes out as the smallest normal, which is
	// exactly the transition C wants, with no separate subnormal path.
	if e < f128MinExp {
		e = f128MinExp
	}
	shift := f128MantBits - e
	n := new(big.Int)
	d := new(big.Int)
	if shift >= 0 {
		n.Lsh(num, uint(shift))
		d.Set(den)
	} else {
		n.Set(num)
		d.Lsh(den, uint(-shift))
	}
	m, rem := new(big.Int).QuoRem(n, d, new(big.Int))
	twice := new(big.Int).Lsh(rem, 1)
	if c := twice.Cmp(d); c > 0 || (c == 0 && m.Bit(0) != 0) {
		m.Add(m, big.NewInt(1))
	}
	if m.BitLen() > f128MantBits+1 { // rounded up past 2^113
		m.Rsh(m, 1)
		e++
	}
	if e > f128ExpBias { // overflow
		return Float128{Hi: sign | uint64(f128ExpMax)<<48}
	}
	var expField uint64
	mant := new(big.Int)
	if m.BitLen() > f128MantBits { // normal: m is in [2^112, 2^113)
		expField = uint64(e + f128ExpBias)
		mant.Sub(m, new(big.Int).Lsh(big.NewInt(1), uint(f128MantBits)))
	} else { // subnormal, or rounded all the way down to zero
		mant.Set(m)
	}
	if expField >= f128ExpMax {
		return Float128{Hi: sign | uint64(f128ExpMax)<<48}
	}
	lo := new(big.Int).And(mant, mask64)
	hiPart := new(big.Int).Rsh(mant, 64)
	return Float128{Hi: sign | expField<<48 | hiPart.Uint64(), Lo: lo.Uint64()}
}

// cmpPow2 compares num/den against 2^e without materialising 2^e.
func cmpPow2(num, den *big.Int, e int) int {
	if e >= 0 {
		return num.Cmp(new(big.Int).Lsh(den, uint(e)))
	}
	return new(big.Int).Lsh(num, uint(-e)).Cmp(den)
}

var mask64 = new(big.Int).SetUint64(math.MaxUint64)

// Float128FromFloat64 widens a float64 to binary128. Every binary64 value --
// finite, infinite, NaN and subnormal -- is represented exactly in binary128,
// so this is a re-encoding, not a rounding.
func Float128FromFloat64(f float64) Float128 {
	bits := math.Float64bits(f)
	sign := uint64(0)
	if bits>>63 != 0 {
		sign = f128SignMask
	}
	exp := int((bits >> 52) & 0x7FF)
	mant := bits & ((uint64(1) << 52) - 1)
	switch exp {
	case 0x7FF:
		// inf keeps a zero mantissa; a NaN carries its payload in the top
		// mantissa bits and is made quiet.
		hi := sign | uint64(f128ExpMax)<<48
		if mant != 0 {
			hi |= f128QuietHi | mant>>5
			return Float128{Hi: hi, Lo: mant << 59}
		}
		return Float128{Hi: hi}
	case 0:
		if mant == 0 {
			return Float128{Hi: sign}
		}
		// A binary64 subnormal is a normal binary128: shift the mantissa up
		// until its leading bit is at the implicit position. That leading bit
		// then has to be dropped -- the normal path below re-encodes `mant`
		// as the 52 stored bits *below* the implicit one, and leaving it set
		// would scale the mantissa field by 2^112 and overflow it.
		shift := 0
		for mant&(uint64(1)<<52) == 0 {
			mant <<= 1
			shift++
		}
		mant &^= uint64(1) << 52
		exp = 1 - shift
	}
	// value = (1.mant) * 2^(exp-1023); scaling the mantissa to 112 bits moves
	// the 52 stored bits to positions 111..60, and the implicit 1 is bit 112.
	e := exp - 1023 + f128ExpBias
	return Float128{Hi: sign | uint64(e)<<48 | mant>>4, Lo: mant << 60}
}
