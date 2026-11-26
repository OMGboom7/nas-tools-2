// Package recognitioncache stores title recognition results and manual corrections.
package recognitioncache

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"unicode/utf8"
)

// tmdb.dat was written by Python's pickle.dump(dict, HIGHEST_PROTOCOL).
// Interpret only the data forms used by this cache. In particular the only
// supported constructor is the literal app.utils.types.MediaType enum.
var errLegacy = errors.New("unsupported or damaged tmdb.dat recognition cache")

const legacyLimit = 64 << 20

type legacyDict struct {
	keys   []string
	values map[string]any
}
type mark struct{}
type mediaTypeClass struct{}
type enumArgument struct{ value string }

func readLegacy(data []byte) (*legacyDict, error) {
	if len(data) < 3 || len(data) > legacyLimit || data[0] != 0x80 || data[1] < 2 || data[1] > 5 {
		return nil, errLegacy
	}
	r := bytes.NewReader(data[2:])
	stack := []any{}
	memo := map[uint32]any{}
	read := func(n int) ([]byte, error) {
		if n < 0 || n > r.Len() {
			return nil, errLegacy
		}
		start := len(data) - r.Len()
		_, err := r.Seek(int64(n), io.SeekCurrent)
		return data[start : start+n], err
	}
	num := func(n int) (uint64, error) {
		b, err := read(n)
		if err != nil {
			return 0, err
		}
		var v uint64
		for i, x := range b {
			v |= uint64(x) << (8 * i)
		}
		return v, nil
	}
	for r.Len() > 0 {
		code, _ := r.ReadByte()
		n := len(stack)
		if n > 1<<18 || len(memo) > 2<<20 {
			return nil, errLegacy
		}
		switch code {
		case 0x95: // FRAME lengths do not change operand interpretation.
			size, err := num(8)
			if err != nil || size > uint64(r.Len()) {
				return nil, errLegacy
			}
		case '}':
			stack = append(stack, &legacyDict{values: map[string]any{}})
		case '(':
			stack = append(stack, mark{})
		case 'N':
			stack = append(stack, nil)
		case 0x88, 0x89:
			stack = append(stack, code == 0x88)
		case 'J', 'K', 'M', 0x8a:
			width := map[byte]int{'J': 4, 'K': 1, 'M': 2}[code]
			if code == 0x8a {
				v, err := num(1)
				if err != nil || v > 8 {
					return nil, errLegacy
				}
				width = int(v)
			}
			v, err := num(width)
			if err != nil {
				return nil, errLegacy
			}
			if code == 'J' {
				v = uint64(int64(int32(v)))
			}
			if code == 0x8a && width > 0 && width < 8 && v>>(width*8-1) != 0 {
				v |= ^uint64(0) << (width * 8)
			}
			stack = append(stack, int64(v))
		case 'G':
			b, err := read(8)
			if err != nil {
				return nil, errLegacy
			}
			v := math.Float64frombits(binary.BigEndian.Uint64(b))
			if math.IsInf(v, 0) || math.IsNaN(v) {
				return nil, errLegacy
			}
			stack = append(stack, v)
		case 'X', 0x8c, 0x8d:
			width := map[byte]int{'X': 4, 0x8c: 1, 0x8d: 8}[code]
			size, err := num(width)
			if err != nil || size > uint64(r.Len()) {
				return nil, errLegacy
			}
			b, err := read(int(size))
			if err != nil || !utf8.Valid(b) {
				return nil, errLegacy
			}
			stack = append(stack, string(b))
		case 'q', 'r', 0x94:
			if n == 0 {
				return nil, errLegacy
			}
			i := uint64(len(memo))
			if code != 0x94 {
				width := 1
				if code == 'r' {
					width = 4
				}
				var err error
				i, err = num(width)
				if err != nil {
					return nil, errLegacy
				}
			}
			memo[uint32(i)] = stack[n-1]
		case 'h', 'j':
			width := 1
			if code == 'j' {
				width = 4
			}
			i, err := num(width)
			if err != nil {
				return nil, errLegacy
			}
			v, ok := memo[uint32(i)]
			if !ok {
				return nil, errLegacy
			}
			stack = append(stack, v)
		case 'c':
			const name = "app.utils.types\nMediaType\n"
			b, err := read(len(name))
			if err != nil || string(b) != name {
				return nil, errLegacy
			}
			stack = append(stack, mediaTypeClass{})
		case 0x93:
			if n < 2 || stack[n-2] != "app.utils.types" || stack[n-1] != "MediaType" {
				return nil, errLegacy
			}
			stack = append(stack[:n-2], mediaTypeClass{})
		case 0x85:
			if n < 1 {
				return nil, errLegacy
			}
			v, ok := stack[n-1].(string)
			if !ok {
				return nil, errLegacy
			}
			stack[n-1] = enumArgument{v}
		case 'R':
			if n < 2 {
				return nil, errLegacy
			}
			_, ok := stack[n-2].(mediaTypeClass)
			arg, valid := stack[n-1].(enumArgument)
			if !ok || !valid || !knownType(arg.value) {
				return nil, errLegacy
			}
			stack = append(stack[:n-2], arg.value)
		case 's', 'u':
			i := n - 2
			if code == 'u' {
				i = n - 1
				for i >= 0 {
					if _, ok := stack[i].(mark); ok {
						break
					}
					i--
				}
			}
			if i < 1 {
				return nil, errLegacy
			}
			d, ok := stack[i-1].(*legacyDict)
			if !ok {
				return nil, errLegacy
			}
			items := stack[i:]
			if code == 'u' {
				items = items[1:]
			}
			if len(items)%2 != 0 {
				return nil, errLegacy
			}
			for j := 0; j < len(items); j += 2 {
				k, ok := items[j].(string)
				if !ok {
					return nil, errLegacy
				}
				if _, exists := d.values[k]; !exists {
					d.keys = append(d.keys, k)
				}
				d.values[k] = items[j+1]
			}
			stack = stack[:i]
		case '.':
			if n != 1 || r.Len() != 0 {
				return nil, errLegacy
			}
			d, ok := stack[0].(*legacyDict)
			if !ok {
				return nil, errLegacy
			}
			return d, nil
		default:
			return nil, errLegacy
		}
	}
	return nil, errLegacy
}
