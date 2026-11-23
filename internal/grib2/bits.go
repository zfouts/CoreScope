package grib2

import (
	"encoding/binary"
	"fmt"
)

// bitReader reads big-endian bit fields from a byte slice. Every read is
// bounds-checked and returns ErrCorrupt rather than panicking.
type bitReader struct {
	buf   []byte
	pos   uint64 // bit offset of the next read
	nbits uint64 // len(buf) * 8
}

func newBitReader(buf []byte) *bitReader {
	return &bitReader{buf: buf, nbits: uint64(len(buf)) * 8}
}

// read returns the next n bits (0..32) as an unsigned integer.
func (r *bitReader) read(n uint) (uint32, error) {
	if n == 0 {
		return 0, nil
	}
	if n > 32 || r.pos+uint64(n) > r.nbits {
		return 0, fmt.Errorf("%w: bit read of %d at %d past %d", ErrCorrupt, n, r.pos, r.nbits)
	}
	first := r.pos >> 3
	shift := uint(r.pos & 7)
	need := (shift + n + 7) / 8 // 1..5 bytes
	var acc uint64
	for i := uint64(0); i < uint64(need); i++ {
		acc = acc<<8 | uint64(r.buf[first+i])
	}
	acc >>= need*8 - shift - n
	r.pos += uint64(n)
	return uint32(acc & (1<<n - 1)), nil
}

// seek moves to an absolute bit offset (must be within the buffer).
func (r *bitReader) seek(pos uint64) error {
	if pos > r.nbits {
		return fmt.Errorf("%w: bit offset %d past %d", ErrCorrupt, pos, r.nbits)
	}
	r.pos = pos
	return nil
}

// roundUp8 rounds a bit count up to a whole number of octets.
func roundUp8(bits uint64) uint64 { return (bits + 7) &^ 7 }

// Octet helpers. Callers check lengths before use; offsets below are
// 0-based positions within a section slice.

func u16(b []byte, off int) uint16 { return binary.BigEndian.Uint16(b[off:]) }
func u32(b []byte, off int) uint32 { return binary.BigEndian.Uint32(b[off:]) }

// GRIB2 signed integers are sign-magnitude: the high bit is the sign.
func s8(b []byte, off int) int {
	v := int(b[off] & 0x7f)
	if b[off]&0x80 != 0 {
		return -v
	}
	return v
}

func s16(b []byte, off int) int {
	v := u16(b, off)
	m := int(v & 0x7fff)
	if v&0x8000 != 0 {
		return -m
	}
	return m
}

func s32(b []byte, off int) int64 {
	v := u32(b, off)
	m := int64(v & 0x7fffffff)
	if v&0x80000000 != 0 {
		return -m
	}
	return m
}

// signMagnitude interprets the low n bits of v (n >= 1) as sign-magnitude.
func signMagnitude(v uint32, n uint) int64 {
	sign := uint32(1) << (n - 1)
	m := int64(v &^ sign)
	if v&sign != 0 {
		return -m
	}
	return m
}
