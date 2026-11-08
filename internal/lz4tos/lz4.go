package lz4tos

// lz4tos packs a linked ELF into the envelope TinyOS's exec() expects:
//
//	0  "LZ4TOS"       6 bytes
//	6  u16 flags      bit0 = the 12-byte window hint follows (required)
//	8  u32 raw_size
//	12 u32 comp_size
//	16 u32 span
//	20 u32 stack_ask
//	24 u32 heap_ask
//	28 LZ4 block data
//
// Both the compressor and the hint derivation are the same algorithms the
// TinyOS build uses (tools/lz4.c and tools/mklfs.c), so a file produced here is
// indistinguishable from one packed by `tos build`.

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
)

const (
	headerSize = 28
	magic      = "LZ4TOS"
	hintBit    = 1
)

const (
	lz4MinMatch     = 4
	lz4MaxOffset    = 65535
	lz4LastLiterals = 5
	lz4MFLimit      = 12
	lz4HashBits     = 14
	lz4HashSize     = 1 << lz4HashBits
	lz4MaxChain     = 32
)

type Hint struct {
	Span     uint32
	StackAsk uint32
	HeapAsk  uint32
}

type Result struct {
	RawSize  int
	CompSize int
	Hint     Hint
}

func lz4CompressBound(n int) int { return n + n/255 + 16 }

func lz4Hash(p []byte) uint32 {
	seq := binary.LittleEndian.Uint32(p)
	return (seq * 2654435761) >> (32 - lz4HashBits)
}

func lz4EmitLen(dst []byte, op, n int) int {
	for n >= 255 {
		dst[op] = 255
		op++
		n -= 255
	}
	dst[op] = byte(n)
	return op + 1
}

func lz4CompressBlock(src, dst []byte) int {
	n := len(src)
	if n == 0 {
		return 0
	}
	head := make([]int32, lz4HashSize)
	for i := range head {
		head[i] = -1
	}
	chain := make([]int32, n)

	lastMatchStart := n - lz4MFLimit
	anchor, ip, op := 0, 0, 0

	for ip <= lastMatchStart {
		h := lz4Hash(src[ip:])
		cand := head[h]
		bestLen, bestOff := 0, 0

		probes := 0
		for c := cand; c >= 0 && probes < lz4MaxChain; probes++ {
			off := ip - int(c)
			if off > lz4MaxOffset {
				break
			}
			maxLen := n - lz4LastLiterals - ip
			if src[int(c)+bestLen] == src[ip+bestLen] &&
				string(src[c:c+lz4MinMatch]) == string(src[ip:ip+lz4MinMatch]) {
				ml := lz4MinMatch
				for ml < maxLen && src[int(c)+ml] == src[ip+ml] {
					ml++
				}
				if ml > bestLen {
					bestLen, bestOff = ml, off
					if ml >= 64 {
						break
					}
				}
			}
			c = chain[c]
		}

		chain[ip] = cand
		head[h] = int32(ip)

		if bestLen < lz4MinMatch {
			ip++
			continue
		}

		lit := ip - anchor
		tokenPos := op
		op++
		if lit >= 15 {
			dst[tokenPos] = 0xF0
			op = lz4EmitLen(dst, op, lit-15)
		} else {
			dst[tokenPos] = byte(lit << 4)
		}
		copy(dst[op:], src[anchor:ip])
		op += lit

		dst[op] = byte(bestOff)
		dst[op+1] = byte(bestOff >> 8)
		op += 2

		mlen := bestLen - lz4MinMatch
		if mlen < 15 {
			dst[tokenPos] |= byte(mlen)
		} else {
			dst[tokenPos] |= 15
			op = lz4EmitLen(dst, op, mlen-15)
		}

		for k := 1; k < bestLen && ip+k <= lastMatchStart; k++ {
			hk := lz4Hash(src[ip+k:])
			chain[ip+k] = head[hk]
			head[hk] = int32(ip + k)
		}
		ip += bestLen
		anchor = ip
	}

	lit := n - anchor
	tokenPos := op
	op++
	if lit >= 15 {
		dst[tokenPos] = 0xF0
		op = lz4EmitLen(dst, op, lit-15)
	} else {
		dst[tokenPos] = byte(lit << 4)
	}
	copy(dst[op:], src[anchor:])
	op += lit
	return op
}

// WindowHint derives span/stack/heap the way mklfs and exec() do.
func WindowHint(raw []byte) (Hint, error) {
	f, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		return Hint{}, err
	}
	defer f.Close()

	var lo, hi uint64 = ^uint64(0), 0
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Memsz == 0 {
			continue
		}
		if p.Vaddr < lo {
			lo = p.Vaddr
		}
		if p.Vaddr+p.Memsz > hi {
			hi = p.Vaddr + p.Memsz
		}
	}
	if hi <= lo {
		return Hint{}, fmt.Errorf("image has no usable PT_LOAD")
	}
	h := Hint{Span: uint32(((hi - lo) + 4095) / 4096 * 4096)}

	for _, p := range f.Progs {
		if p.Type != elf.PT_NOTE || p.Off+p.Filesz > uint64(len(raw)) {
			continue
		}
		b := raw[p.Off : p.Off+p.Filesz]
		for uint64(len(b)) >= 12 {
			namesz := binary.LittleEndian.Uint32(b)
			descsz := binary.LittleEndian.Uint32(b[4:])
			ntype := binary.LittleEndian.Uint32(b[8:])
			descOff := uint64(12) + ((uint64(namesz) + 3) &^ 3)
			next := descOff + ((uint64(descsz) + 3) &^ 3)
			if descOff < 12 || next > uint64(len(b)) || next <= 12 {
				break
			}
			if namesz >= 6 && descsz >= 4 && uint64(len(b)) >= 18 &&
				string(b[12:18]) == "TinyOS" {
				v := binary.LittleEndian.Uint32(b[descOff:])
				if ntype == 1 && h.StackAsk == 0 {
					h.StackAsk = v
				} else if ntype == 2 && h.HeapAsk == 0 {
					h.HeapAsk = v
				}
			}
			b = b[next:]
		}
	}
	return h, nil
}

// Pack compresses elfPath into the LZ4TOS envelope at outPath.
func Pack(elfPath, outPath string) (Result, error) {
	raw, err := os.ReadFile(elfPath)
	if err != nil {
		return Result{}, err
	}
	hint, err := WindowHint(raw)
	if err != nil {
		return Result{}, fmt.Errorf("window hint: %w", err)
	}
	dst := make([]byte, lz4CompressBound(len(raw)))
	clen := lz4CompressBlock(raw, dst)
	if clen <= 0 {
		return Result{}, fmt.Errorf("compression produced no data")
	}
	hdr := make([]byte, headerSize)
	copy(hdr, magic)
	hdr[6] = hintBit
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(raw)))
	binary.LittleEndian.PutUint32(hdr[12:], uint32(clen))
	binary.LittleEndian.PutUint32(hdr[16:], hint.Span)
	binary.LittleEndian.PutUint32(hdr[20:], hint.StackAsk)
	binary.LittleEndian.PutUint32(hdr[24:], hint.HeapAsk)

	out := make([]byte, 0, headerSize+clen)
	out = append(out, hdr...)
	out = append(out, dst[:clen]...)
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		return Result{}, err
	}
	return Result{RawSize: len(raw), CompSize: clen, Hint: hint}, nil
}
