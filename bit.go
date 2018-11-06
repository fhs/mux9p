package main

import "9fans.net/go/plan9"

func gbit8(b []byte) (uint8, []byte) {
	return uint8(b[0]), b[1:]
}

func gbit16(b []byte) (uint16, []byte) {
	return uint16(b[0]) | uint16(b[1])<<8, b[2:]
}

func gbit32(b []byte) (uint32, []byte) {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24, b[4:]
}

func gbit64(b []byte) (uint64, []byte) {
	lo, b := gbit32(b)
	hi, b := gbit32(b)
	return uint64(hi)<<32 | uint64(lo), b
}

func gstring(b []byte) (string, []byte) {
	n, b := gbit16(b)
	return string(b[0:n]), b[n:]
}

func pbit8(b []byte, x uint8) int {
	b[0] = x
	return 1
}

func pbit16(b []byte, x uint16) int {
	b[0] = byte(x)
	b[0+1] = byte(x >> 8)
	return 2
}

func pbit32(b []byte, x uint32) int {
	b[0] = byte(x)
	b[1] = byte(x >> 8)
	b[2] = byte(x >> 16)
	b[3] = byte(x >> 24)
	return 4
}

func pbit64(b []byte, x uint64) int {
	i := pbit32(b, uint32(x))
	i += pbit32(b[i:], uint32(x>>32))
	return i
}

func pstring(b []byte, s string) int {
	if len(s) >= 1<<16 || 2+len(s) > len(b) {
		panic(plan9.ProtocolError("string too long"))
	}
	i := pbit16(b, uint16(len(s)))
	i += copy(b[i:], []byte(s))
	return i
}
