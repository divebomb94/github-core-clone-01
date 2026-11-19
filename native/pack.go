package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strconv"
	"sync"
)

var packObjectHashPool = sync.Pool{New: func() any { return sha1.New() }}

// PktItem is a line, flush, delim, or response-end packet.
type PktItem struct {
	Type string
	Data []byte
}

func pktLine(data []byte) ([]byte, error) {
	if len(data) > 0xffff-4 {
		return nil, fmt.Errorf("Pkt-line too long")
	}
	return append([]byte(fmt.Sprintf("%04x", len(data)+4)), data...), nil
}
func pktFlush() []byte       { return []byte("0000") }
func pktDelim() []byte       { return []byte("0001") }
func pktResponseEnd() []byte { return []byte("0002") }

func parsePktLines(buf []byte, offset int) ([]PktItem, error) {
	if offset < 0 || offset > len(buf) {
		return nil, fmt.Errorf("Invalid pkt-line offset")
	}
	items := []PktItem{}
	for offset < len(buf) {
		if len(buf)-offset < 4 {
			return nil, fmt.Errorf("Malformed pkt-line length")
		}
		prefix := buf[offset : offset+4]
		for _, b := range prefix {
			if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F') {
				return nil, fmt.Errorf("Malformed pkt-line length")
			}
		}
		length, _ := strconv.ParseUint(string(prefix), 16, 16)
		offset += 4
		if length <= 2 {
			items = append(items, PktItem{Type: []string{"flush", "delim", "response-end"}[length]})
		} else {
			n := int(length) - 4
			if n < 0 || n > len(buf)-offset {
				return nil, fmt.Errorf("Malformed pkt-line length")
			}
			items = append(items, PktItem{Type: "line", Data: buf[offset : offset+n]})
			offset += n
		}
	}
	return items, nil
}

var packTypeNames = map[byte]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}
var packTypeCodes = map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

var packWriters = sync.Pool{New: func() any { return zlib.NewWriter(io.Discard) }}
var packReaders sync.Pool

// buildPack preserves the first occurrence of each requested SHA and writes full objects.
func buildPack(shas []string, objects map[string]GitObject) ([]byte, error) {
	unique := make([]string, 0, len(shas))
	seen := make(map[string]bool)
	for _, sha := range shas {
		if !seen[sha] {
			seen[sha] = true
			unique = append(unique, sha)
		}
	}
	var out bytes.Buffer
	out.Grow(12 + len(unique)*128 + 20)
	zw := packWriters.Get().(*zlib.Writer)
	defer packWriters.Put(zw)
	out.WriteString("PACK")
	if uint64(len(unique)) > 0xffffffff {
		return nil, fmt.Errorf("Too many pack objects")
	}
	var header [8]byte
	binary.BigEndian.PutUint32(header[:4], 2)
	binary.BigEndian.PutUint32(header[4:], uint32(len(unique)))
	out.Write(header[:])
	for _, sha := range unique {
		object, ok := objects[sha]
		if !ok {
			return nil, fmt.Errorf("Missing object %s", sha)
		}
		code, ok := packTypeCodes[object.Type]
		if !ok {
			return nil, fmt.Errorf("Invalid pack object type")
		}
		size := uint64(len(object.Content))
		b := code<<4 | byte(size&15)
		size >>= 4
		for size != 0 {
			out.WriteByte(b | 128)
			b = byte(size & 127)
			size >>= 7
		}
		out.WriteByte(b)
		zw.Reset(&out)
		if _, err := zw.Write(object.Content); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
	}
	checksum := sha1.Sum(out.Bytes())
	out.Write(checksum[:])
	return out.Bytes(), nil
}

// inflateExact consumes just one zlib stream. bytes.Reader implements io.ByteReader,
// preventing the inflater from buffering bytes belonging to the next entry.
func inflateExact(buf []byte, start int) ([]byte, int, error) {
	if start < 0 || start >= len(buf) {
		return nil, 0, fmt.Errorf("Invalid compressed stream offset")
	}
	reader := bytes.NewReader(buf[start:])
	var zr io.ReadCloser
	var err error
	if cached := packReaders.Get(); cached != nil {
		zr = cached.(io.ReadCloser)
		err = zr.(zlib.Resetter).Reset(reader, nil)
	} else {
		zr, err = zlib.NewReader(reader)
	}
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	closeErr := zr.Close()
	if err != nil {
		return nil, 0, err
	}
	if closeErr != nil {
		return nil, 0, closeErr
	}
	packReaders.Put(zr)
	return data, len(buf) - reader.Len(), nil
}

const packMaxSafeInteger uint64 = 1<<53 - 1

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	next := func() (byte, error) {
		if pos >= len(delta) {
			return 0, fmt.Errorf("Truncated delta")
		}
		b := delta[pos]
		pos++
		return b, nil
	}
	size := func() (uint64, error) {
		var value uint64
		factor := uint64(1)
		for {
			b, err := next()
			if err != nil {
				return 0, err
			}
			digit := uint64(b & 127)
			if factor > packMaxSafeInteger || digit > (packMaxSafeInteger-value)/factor {
				return 0, fmt.Errorf("Delta size overflow")
			}
			value += digit * factor
			if b&128 == 0 {
				return value, nil
			}
			factor *= 128
		}
	}
	baseSize, err := size()
	if err != nil {
		return nil, err
	}
	if baseSize != uint64(len(base)) {
		return nil, fmt.Errorf("Delta base size mismatch")
	}
	target, err := size()
	if err != nil {
		return nil, err
	}
	var out []byte
	for pos < len(delta) {
		op, _ := next()
		var chunk []byte
		if op&128 != 0 {
			var offset, length uint64
			for i := 0; i < 4; i++ {
				if op&(1<<i) != 0 {
					b, e := next()
					if e != nil {
						return nil, e
					}
					offset += uint64(b) << uint(8*i)
				}
			}
			for i := 0; i < 3; i++ {
				if op&(1<<(4+i)) != 0 {
					b, e := next()
					if e != nil {
						return nil, e
					}
					length += uint64(b) << uint(8*i)
				}
			}
			if length == 0 {
				length = 0x10000
			}
			if offset+length > uint64(len(base)) {
				return nil, fmt.Errorf("Delta copy outside base")
			}
			chunk = base[int(offset):int(offset+length)]
		} else {
			if op == 0 || int(op) > len(delta)-pos {
				return nil, fmt.Errorf("Invalid delta insert")
			}
			chunk = delta[pos : pos+int(op)]
			pos += int(op)
		}
		if uint64(len(out))+uint64(len(chunk)) > target {
			return nil, fmt.Errorf("Delta exceeds target size")
		}
		out = append(out, chunk...)
	}
	if uint64(len(out)) != target {
		return nil, fmt.Errorf("Delta target size mismatch")
	}
	return out, nil
}

// PackResult lists reconstructed SHAs in entry order, including duplicates.
type PackResult struct {
	SHAs  []string
	Count uint32
}

// parsePack resolves bases in encounter order, just as pack.ts does. Store writes
// are deferred until every entry and the trailing position have been validated.
func parsePack(buf []byte, store *GitStore) (PackResult, error) {
	fail := func(message string) (PackResult, error) { return PackResult{}, fmt.Errorf("%s", message) }
	if len(buf) < 32 || string(buf[:4]) != "PACK" {
		return fail("Invalid pack header")
	}
	version := binary.BigEndian.Uint32(buf[4:8])
	count := binary.BigEndian.Uint32(buf[8:12])
	if version != 2 && version != 3 {
		return fail("Unsupported pack version")
	}
	end := len(buf) - 20
	checksum := sha1.Sum(buf[:end])
	if !bytes.Equal(checksum[:], buf[end:]) {
		return fail("Pack checksum mismatch")
	}
	pos := 12
	next := func() (byte, error) {
		if pos >= end {
			return 0, fmt.Errorf("Truncated pack")
		}
		b := buf[pos]
		pos++
		return b, nil
	}
	offsets := make(map[int]GitObject)
	resolved := make(map[string]GitObject)
	result := PackResult{SHAs: []string{}, Count: count}
	var hashBuf [sha1.Size]byte
	for i := uint32(0); i < count; i++ {
		entryOffset := pos
		b, err := next()
		if err != nil {
			return PackResult{}, err
		}
		code := (b >> 4) & 7
		size, factor := uint64(b&15), uint64(16)
		for b&128 != 0 {
			b, err = next()
			if err != nil {
				return PackResult{}, err
			}
			digit := uint64(b & 127)
			if factor > packMaxSafeInteger || digit > (packMaxSafeInteger-size)/factor {
				return fail("Pack size overflow")
			}
			size += digit * factor
			factor *= 128
		}
		var base GitObject
		if code == 6 {
			b, err = next()
			if err != nil {
				return PackResult{}, err
			}
			distance := uint64(b & 127)
			for b&128 != 0 {
				b, err = next()
				if err != nil {
					return PackResult{}, err
				}
				if distance+1 > (packMaxSafeInteger-uint64(b&127))/128 {
					return fail("Pack offset overflow")
				}
				distance = (distance+1)*128 + uint64(b&127)
			}
			var ok bool
			if distance <= uint64(entryOffset) {
				base, ok = offsets[entryOffset-int(distance)]
			}
			if !ok {
				return fail("Missing OFS_DELTA base")
			}
		} else if code == 7 {
			if end-pos < 20 {
				return fail("Truncated REF_DELTA")
			}
			sha := hex.EncodeToString(buf[pos : pos+20])
			pos += 20
			var ok bool
			base, ok = resolved[sha]
			if !ok {
				base, ok = store.Objects[sha]
			}
			if !ok {
				return fail("Missing REF_DELTA base " + sha)
			}
		} else if packTypeNames[code] == "" {
			return fail("Invalid pack object type")
		}
		data, nextPos, err := inflateExact(buf[:end], pos)
		if err != nil {
			return PackResult{}, err
		}
		pos = nextPos
		if uint64(len(data)) != size {
			return fail("Pack object size mismatch")
		}
		object := GitObject{Type: packTypeNames[code], Content: data}
		if code == 6 || code == 7 {
			object.Type = base.Type
			object.Content, err = applyDelta(base.Content, data)
			if err != nil {
				return PackResult{}, err
			}
		}
		h := packObjectHashPool.Get().(hash.Hash)
		h.Reset()
		h.Write(append(hashBuf[:0], object.Type...))
		h.Write(append(hashBuf[:0], ' '))
		h.Write(append(hashBuf[:0], strconv.Itoa(len(object.Content))...))
		h.Write(append(hashBuf[:0], 0))
		h.Write(object.Content)
		sha := hex.EncodeToString(h.Sum(hashBuf[:0]))
		packObjectHashPool.Put(h)
		offsets[entryOffset] = object
		resolved[sha] = object
		result.SHAs = append(result.SHAs, sha)
	}
	if pos != end {
		return fail("Unexpected data after pack entries")
	}
	if store.Objects == nil {
		store.Objects = make(map[string]GitObject, len(resolved))
	}
	for sha, object := range resolved {
		store.Objects[sha] = object
	}
	return result, nil
}
