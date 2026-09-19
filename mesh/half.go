package mesh

import "math"

// The peer frame carries the battery voltage as a struct 'e' half float.
// These are ports of mp_decode_half_float and the software
// mp_encode_half_float in MicroPython v1.25.0 py/binary.c (the ESP32 build
// has no native _Float16), so an encoded voltage matches a Totem's byte for
// byte. Like the original, the encoder rounds half up, lets a mantissa
// carry spill into the exponent field and flushes values that should
// become the largest subnormals (f32 exponent 112) to zero.

func encodeHalf(f float32) uint16 {
	i := math.Float32bits(f)
	m := uint16(i>>13) & 0x3ff
	if i&(1<<12) != 0 {
		m++
	}
	e := int(i>>23) & 0xff
	switch {
	case e == 0xff:
		e = 0x1f
	case e != 0:
		e -= 127 - 15
		if e < 0 {
			if e >= -11 {
				m = (m | 0x400) >> -e
				if m&1 != 0 {
					m = m>>1 + 1
				} else {
					m >>= 1
				}
			} else {
				m = 0
			}
			e = 0
		} else if e > 0x3f {
			e = 0x1f
			m = 0
		}
	}
	return uint16(i>>16)&0x8000 | uint16(e<<10) | m
}

func decodeHalf(h uint16) float32 {
	m := uint32(h) & 0x3ff
	e := int(h>>10) & 0x1f
	switch {
	case e == 0x1f:
		e = 0xff
	case e != 0:
		e += 127 - 15
	case m != 0:
		e = 127 - 15
		for m&0x400 == 0 {
			m <<= 1
			e--
		}
		m -= 0x400
		e++
	}
	return math.Float32frombits(uint32(h&0x8000)<<16 | uint32(e)<<23 | m<<13)
}
