package customhosts

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"strings"
	"unicode/utf8"
)

const BeginMarker = "# CustomHostsPlugin"
const EndMarker = "# End CustomHostsPlugin"
const maxFile = 1 << 20

var ErrInput = errors.New("invalid hosts configuration")
var ErrMarkers = errors.New("ambiguous custom hosts markers")
var ErrFile = errors.New("hosts target is not the expected regular file")

type InvalidLine struct {
	Line int
	Text string
}
type Result struct {
	Applied     bool
	Invalid     []InvalidLine
	LegacyBlock bool
}

var writeGate = make(chan struct{}, 1)

// Parse accepts IPv4/IPv6 with one or more host aliases, blank lines and
// comments. Invalid mappings are reported individually, not written to hosts.
func Parse(ctx context.Context, input string) ([]string, []InvalidLine, error) {
	if len(input) > 64<<10 || !utf8.ValidString(input) || strings.ContainsRune(input, '\x00') {
		return nil, nil, ErrInput
	}
	valid := []string{}
	invalid := []InvalidLine{}
	lines := strings.Split(input, "\n")
	if len(lines) > 4096 {
		return nil, nil, ErrInput
	}
	for number, line := range lines {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		content, _, _ := strings.Cut(strings.TrimSpace(line), "#")
		fields := strings.Fields(content)
		if len(fields) == 0 {
			continue
		}
		address, err := netip.ParseAddr(fields[0])
		ok := err == nil && address.Zone() == "" && len(fields) > 1 && len(fields) <= 65
		if ok {
			for _, name := range fields[1:] {
				if !validHostname(name) {
					ok = false
					break
				}
			}
		}
		if !ok {
			invalid = append(invalid, InvalidLine{Line: number + 1, Text: line})
			continue
		}
		valid = append(valid, address.String()+"\t"+strings.Join(fields[1:], " "))
	}
	return valid, invalid, nil
}

func validHostname(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	name = strings.TrimSuffix(name, ".")
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
				return false
			}
		}
	}
	return true
}

// Rewrite preserves bytes outside the managed block. A legacy begin-only
// marker owns the remainder, matching the old plugin's append-only layout.
// New writes add an end marker so later manual entries remain untouched.
func Rewrite(original string, mappings []string) (string, bool, error) {
	for _, mapping := range mappings {
		if strings.ContainsAny(mapping, "\r\n\x00") {
			return "", false, ErrInput
		}
		valid, invalid, err := Parse(context.Background(), mapping)
		if err != nil || len(invalid) > 0 || len(valid) != 1 {
			return "", false, ErrInput
		}
	}
	if len(original) > maxFile || !utf8.ValidString(original) || strings.ContainsRune(original, '\x00') {
		return "", false, ErrFile
	}
	begin, end, offset := -1, -1, 0
	for _, line := range strings.SplitAfter(original, "\n") {
		switch strings.TrimSpace(line) {
		case BeginMarker:
			if begin >= 0 {
				return "", false, ErrMarkers
			}
			begin = offset
		case EndMarker:
			if begin < 0 || end >= 0 {
				return "", false, ErrMarkers
			}
			end = offset + len(line)
		}
		offset += len(line)
	}
	if len(mappings) == 0 {
		return original, false, nil
	}
	prefix, suffix, legacy := original, "", false
	if begin >= 0 {
		prefix = original[:begin]
		if end >= 0 {
			suffix = original[end:]
		} else {
			legacy = true
		}
	}
	if prefix != "" && !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	result := prefix + BeginMarker + "\n" + strings.Join(mappings, "\n") + "\n" + EndMarker + "\n" + suffix
	if len(result) > maxFile {
		return "", false, ErrFile
	}
	return result, legacy, nil
}

// Apply writes the existing inode, supporting Docker's bind-mounted /etc/hosts.
// It never creates a target or follows symlinks. Failed writes attempt to restore
// the original bytes; callers must surface an error, not report active state.
func Apply(ctx context.Context, path, input string) (Result, error) {
	mappings, invalid, err := Parse(ctx, input)
	result := Result{Invalid: invalid}
	if err != nil || len(mappings) == 0 {
		return result, err
	}
	select {
	case writeGate <- struct{}{}:
	case <-ctx.Done():
		return result, ctx.Err()
	}
	defer func() { <-writeGate }()
	info, err := os.Lstat(path)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, ErrFile
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return result, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return result, ErrFile
	}
	if err := lockFile(file); err != nil {
		return result, err
	}
	defer unlockFile(file)
	contents, err := io.ReadAll(io.LimitReader(file, maxFile+1))
	if err != nil || len(contents) > maxFile {
		return result, ErrFile
	}
	next, legacy, err := Rewrite(string(contents), mappings)
	if err != nil {
		return result, err
	}
	result.LegacyBlock = legacy
	if err := ctx.Err(); err != nil {
		return result, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(actual, current) {
		return result, ErrFile
	}
	if next != string(contents) {
		if err := replaceWithRollback(ctx, file, []byte(next), contents); err != nil {
			return result, err
		}
	}
	current, err = os.Lstat(path)
	if err != nil || !os.SameFile(actual, current) {
		return result, ErrFile
	}
	result.Applied = true
	return result, nil
}

type writableHosts interface {
	Seek(int64, int) (int64, error)
	Write([]byte) (int, error)
	Truncate(int64) error
	Sync() error
}

func replaceWithRollback(ctx context.Context, file writableHosts, next, original []byte) error {
	if err := replaceContents(ctx, file, next); err != nil {
		return errors.Join(err, replaceContents(context.Background(), file, original))
	}
	return nil
}

func replaceContents(ctx context.Context, file writableHosts, contents []byte) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for len(contents) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, err := file.Write(contents)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		contents = contents[written:]
	}
	position, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file.Truncate(position); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return file.Sync()
}
