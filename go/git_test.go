package main

import (
	"bytes"
	"github.com/klauspost/compress/zlib"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func TestInflateOneExact(t *testing.T) {
	// Build a pack with several objects, then decode and check every
	// object's content and the exact consumed lengths.
	blobs := [][]byte{
		[]byte("hello world\n"),
		bytes.Repeat([]byte("abcdefgh"), 500),
		[]byte{},
		[]byte("x"),
	}
	rnd := make([]byte, 5000)
	if _, err := rand.Read(rnd); err != nil {
		t.Fatal(err)
	}
	blobs = append(blobs, rnd)

	var objs []PackObject
	for _, b := range blobs {
		objs = append(objs, PackObject{Type: ObjBlob, Content: b})
	}
	pack := encodePack(objs)
	decoded, err := decodePack(pack, func(oid string) *GitObject { return nil })
	if err != nil {
		t.Fatalf("decodePack: %v", err)
	}
	if len(decoded) != len(blobs) {
		t.Fatalf("decoded %d objects, want %d", len(decoded), len(blobs))
	}
	for _, b := range blobs {
		oid := hashObject(ObjBlob, b)
		got, ok := decoded[oid]
		if !ok {
			t.Fatalf("missing blob %s", oid)
		}
		if !bytes.Equal(got.Content, b) {
			t.Fatalf("blob %s content mismatch", oid)
		}
	}
}

func TestPackDeltaRoundTrip(t *testing.T) {
	base := []byte("The quick brown fox jumps over the lazy dog. " + string(bytes.Repeat([]byte("0123456789"), 20)))
	target := []byte("The quick red fox jumps over the lazy dog. " + string(bytes.Repeat([]byte("0123456789"), 20)))
	delta := buildDelta(t, base, target)

	// Build a pack by hand: full base blob + ref-delta against it.
	var pack bytes.Buffer
	pack.WriteString("PACK")
	pack.Write([]byte{0, 0, 0, 2, 0, 0, 0, 2})
	pack.Write(encodeObjectHeader(ObjBlob, len(base)))
	zw, _ := zlib.NewWriterLevel(&pack, 6)
	zw.Write(base)
	zw.Close()
	// ref-delta header: type code 7, size = len(delta) (result size).
	pack.Write(encodePackHeader(7, len(delta)))
	baseOID, _ := hex.DecodeString(hashObject(ObjBlob, base))
	pack.Write(baseOID)
	zw, _ = zlib.NewWriterLevel(&pack, 6)
	zw.Write(delta)
	zw.Close()

	decoded, err := decodePack(pack.Bytes(), func(oid string) *GitObject { return nil })
	if err != nil {
		t.Fatalf("decodePack: %v", err)
	}
	targetOID := hashObject(ObjBlob, target)
	got, ok := decoded[targetOID]
	if !ok {
		t.Fatalf("missing delta-resolved blob %s", targetOID)
	}
	if !bytes.Equal(got.Content, target) {
		t.Fatalf("delta-resolved content mismatch: got %q want %q", got.Content, target)
	}
}

// encodePackHeader writes a pack object header for a raw type code.
func encodePackHeader(code byte, size int) []byte {
	var out []byte
	b := (code << 4) | byte(size&0x0f)
	size >>= 4
	if size > 0 {
		b |= 0x80
	}
	out = append(out, b)
	for size > 0 {
		b = byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

// buildDelta is a minimal delta encoder: common prefix + insert + common suffix.
func buildDelta(t *testing.T, base, target []byte) []byte {
	t.Helper()
	prefix := 0
	for prefix < len(base) && prefix < len(target) && base[prefix] == target[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(base)-prefix && suffix < len(target)-prefix && base[len(base)-1-suffix] == target[len(target)-1-suffix] {
		suffix++
	}
	var out []byte
	writeVarint := func(v int) {
		for {
			b := byte(v & 0x7f)
			v >>= 7
			if v == 0 {
				out = append(out, b)
				return
			}
			out = append(out, b|0x80)
		}
	}
	writeVarint(len(base))
	writeVarint(len(target))
	emitCopy := func(off, size int) {
		op := byte(0x80)
		var extra []byte
		tmp := off
		for i := 0; i < 4; i++ {
			if tmp&0xff != 0 {
				op |= 1 << i
				extra = append(extra, byte(tmp&0xff))
			}
			tmp >>= 8
		}
		tmp = size
		for i := 0; i < 3; i++ {
			if tmp&0xff != 0 {
				op |= 0x10 << i
				extra = append(extra, byte(tmp&0xff))
			}
			tmp >>= 8
		}
		out = append(out, op)
		out = append(out, extra...)
	}
	if prefix > 0 {
		emitCopy(0, prefix)
	}
	mid := target[prefix : len(target)-suffix]
	for len(mid) > 0 {
		n := len(mid)
		if n > 127 {
			n = 127
		}
		out = append(out, byte(n))
		out = append(out, mid[:n]...)
		mid = mid[n:]
	}
	if suffix > 0 {
		emitCopy(len(base)-suffix, suffix)
	}
	return out
}
