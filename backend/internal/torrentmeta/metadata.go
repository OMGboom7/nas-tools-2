package torrentmeta

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid torrent metainfo")

type Metadata struct {
	Name     string
	InfoHash string
}

const maxDepth = 64

// Parse validates a bencoded metainfo dictionary and hashes the exact encoded
// info value, as required for torrent identity. It never decodes payload files.
func Parse(data []byte) (Metadata, error) {
	if len(data) < 10 || len(data) > 8<<20 || data[0] != 'd' {
		return Metadata{}, ErrInvalid
	}
	position := 1
	var info []byte
	for position < len(data) && data[position] != 'e' {
		key, ok := readString(data, &position)
		if !ok {
			return Metadata{}, ErrInvalid
		}
		start := position
		if !readValue(data, &position, 1) {
			return Metadata{}, ErrInvalid
		}
		if string(key) == "info" {
			if info != nil || data[start] != 'd' {
				return Metadata{}, ErrInvalid
			}
			info = data[start:position]
		}
	}
	if position != len(data)-1 || data[position] != 'e' || len(info) == 0 {
		return Metadata{}, ErrInvalid
	}
	name, v2, hasV1Pieces, ok := infoFields(info)
	if !ok || name == "" {
		return Metadata{}, ErrInvalid
	}
	if v2 && !hasV1Pieces {
		hash := sha256.Sum256(info)
		return Metadata{Name: name, InfoHash: hex.EncodeToString(hash[:])}, nil
	}
	hash := sha1.Sum(info)
	return Metadata{Name: name, InfoHash: hex.EncodeToString(hash[:])}, nil
}

func infoFields(info []byte) (string, bool, bool, bool) {
	position := 1
	name, fallback := "", ""
	v2, hasPieces := false, false
	for position < len(info)-1 {
		key, ok := readString(info, &position)
		if !ok {
			return "", false, false, false
		}
		start := position
		if !readValue(info, &position, 1) {
			return "", false, false, false
		}
		switch string(key) {
		case "name", "name.utf-8":
			valuePosition := start
			value, ok := readString(info, &valuePosition)
			if !ok || valuePosition != position || !utf8.Valid(value) {
				return "", false, false, false
			}
			if string(key) == "name.utf-8" {
				name = strings.TrimSpace(string(value))
			} else {
				fallback = strings.TrimSpace(string(value))
			}
		case "meta version":
			v2 = string(info[start:position]) == "i2e"
		case "pieces":
			hasPieces = true
		}
	}
	if name == "" {
		name = fallback
	}
	return name, v2, hasPieces, true
}

func readValue(data []byte, position *int, depth int) bool {
	if depth > maxDepth || *position >= len(data) {
		return false
	}
	switch data[*position] {
	case 'd':
		*position++
		for *position < len(data) && data[*position] != 'e' {
			if _, ok := readString(data, position); !ok || !readValue(data, position, depth+1) {
				return false
			}
		}
		if *position >= len(data) {
			return false
		}
		*position++
		return true
	case 'l':
		*position++
		for *position < len(data) && data[*position] != 'e' {
			if !readValue(data, position, depth+1) {
				return false
			}
		}
		if *position >= len(data) {
			return false
		}
		*position++
		return true
	case 'i':
		*position++
		start := *position
		for *position < len(data) && data[*position] != 'e' {
			*position++
		}
		if *position >= len(data) || *position == start {
			return false
		}
		value := data[start:*position]
		if len(value) > 1 && value[0] == '0' || len(value) > 2 && value[0] == '-' && value[1] == '0' {
			return false
		}
		if _, err := strconv.ParseInt(string(value), 10, 64); err != nil {
			return false
		}
		*position++
		return true
	default:
		_, ok := readString(data, position)
		return ok
	}
}

func readString(data []byte, position *int) ([]byte, bool) {
	start := *position
	for *position < len(data) && data[*position] >= '0' && data[*position] <= '9' {
		*position++
	}
	if *position == start || *position >= len(data) || data[*position] != ':' || *position-start > 9 || *position-start > 1 && data[start] == '0' {
		return nil, false
	}
	length, err := strconv.Atoi(string(data[start:*position]))
	if err != nil || length < 0 {
		return nil, false
	}
	*position++
	if length > len(data)-*position {
		return nil, false
	}
	value := data[*position : *position+length]
	*position += length
	return value, true
}
