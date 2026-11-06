package torrentmeta

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

// TransferMetadata contains the exact input identity and a transport copy.
// Tracker URLs and encoded metadata may contain private credentials; they are
// never part of a public JSON response or a persistent progress message.
type TransferMetadata struct {
	Metadata
	Files        []torrentfiles.File
	Private      bool
	TrackerCount int
	Patched      bool
	Content      []byte   `json:"-"`
	TrackerURLs  []string `json:"-"`
}

// PrepareTransfer repairs only missing outer tracker fields, preferring the
// torrent's existing tiers over the matching qBittorrent resume file. The raw
// info dictionary is copied verbatim, including for v2/hybrid metainfo.
func PrepareTransfer(data, resume []byte) (TransferMetadata, error) {
	meta, files, err := Manifest(data)
	if err != nil {
		return TransferMetadata{}, err
	}
	pos, nodes := 0, 0
	value, ok := manifestNode(data, &pos, 0, &nodes)
	if !ok || pos != len(data) {
		return TransferMetadata{}, ErrInvalid
	}
	root, ok := value.(map[string]any)
	if !ok {
		return TransferMetadata{}, ErrInvalid
	}
	info, ok := root["info"].(map[string]any)
	if !ok {
		return TransferMetadata{}, ErrInvalid
	}
	prepared := TransferMetadata{Metadata: meta, Files: files, Content: bytes.Clone(data), TrackerURLs: []string{}}
	if flag, exists := info["private"]; exists {
		n, ok := flag.(int64)
		if !ok || n != 0 && n != 1 {
			return TransferMetadata{}, ErrInvalid
		}
		prepared.Private = n == 1
	}
	announce := ""
	if raw, exists := root["announce"]; exists {
		var ok bool
		announce, ok = raw.(string)
		if !ok {
			return TransferMetadata{}, ErrInvalid
		}
		if announce != "" && !validTransferTracker(announce) {
			return TransferMetadata{}, ErrInvalid
		}
	}
	tiers := [][]string{}
	if raw, exists := root["announce-list"]; exists {
		tiers, err = transferTiers(raw)
		if err != nil {
			return TransferMetadata{}, err
		}
	}
	if announce != "" {
		prepared.TrackerURLs = append(prepared.TrackerURLs, announce)
	}
	for _, tier := range tiers {
		prepared.TrackerURLs = append(prepared.TrackerURLs, tier...)
	}
	if announce != "" {
		prepared.TrackerURLs = uniqueTrackers(prepared.TrackerURLs)
		prepared.TrackerCount = len(prepared.TrackerURLs)
		return prepared, nil
	}
	original, err := transferTopValues(data)
	if err != nil {
		return TransferMetadata{}, err
	}
	if len(tiers) == 0 && len(resume) > 0 {
		if len(resume) > 8<<20 {
			return TransferMetadata{}, ErrInvalid
		}
		pos, nodes = 0, 0
		value, ok := manifestNode(resume, &pos, 0, &nodes)
		if !ok || pos != len(resume) {
			return TransferMetadata{}, ErrInvalid
		}
		state, ok := value.(map[string]any)
		if !ok {
			return TransferMetadata{}, ErrInvalid
		}
		v1, v2 := sha1.Sum(original["info"]), sha256.Sum256(original["info"])
		for key, expected := range map[string][]byte{"info-hash": v1[:], "info-hash2": v2[:]} {
			if raw, exists := state[key]; exists {
				hash, ok := raw.(string)
				if !ok || !bytes.Equal([]byte(hash), expected) {
					return TransferMetadata{}, ErrInvalid
				}
			}
		}
		tiers, err = transferTiers(state["trackers"])
		if err != nil {
			return TransferMetadata{}, err
		}
	}
	if len(tiers) == 0 {
		// Trackerless public torrents may use the original DHT behavior. A private
		// torrent without any original tracker evidence cannot be repaired by guess.
		if prepared.Private {
			return TransferMetadata{}, ErrInvalid
		}
		return prepared, nil
	}
	changes := map[string][]byte{"announce": transferString(tiers[0][0])}
	if len(prepared.TrackerURLs) == 0 {
		changes["announce-list"] = encodeTransferTiers(tiers)
	}
	rewritten, err := patchTransferRoot(original, changes)
	if err != nil {
		return TransferMetadata{}, err
	}
	checked, checkedFiles, err := Manifest(rewritten)
	if err != nil || checked != meta || !bytes.Equal(original["info"], mustTransferInfo(rewritten)) || !sameTransferFiles(files, checkedFiles) {
		return TransferMetadata{}, ErrInvalid
	}
	prepared.Content = rewritten
	prepared.Patched = true
	prepared.TrackerURLs = []string{}
	for _, tier := range tiers {
		prepared.TrackerURLs = append(prepared.TrackerURLs, tier...)
	}
	prepared.TrackerURLs = uniqueTrackers(prepared.TrackerURLs)
	prepared.TrackerCount = len(prepared.TrackerURLs)
	return prepared, nil
}
func validTransferTracker(raw string) bool {
	if raw == "" || len(raw) > 4096 || strings.ContainsAny(raw, "\x00\r\n\t") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Hostname() != "" && u.Fragment == "" && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "udp")
}
func transferTiers(raw any) ([][]string, error) {
	if raw == nil {
		return [][]string{}, nil
	}
	list, ok := raw.([]any)
	if !ok || len(list) > 128 {
		return nil, ErrInvalid
	}
	tiers := [][]string{}
	count := 0
	for _, rawTier := range list {
		values, ok := rawTier.([]any)
		if !ok || len(values) > 128 {
			return nil, ErrInvalid
		}
		tier := []string{}
		for _, value := range values {
			tracker, ok := value.(string)
			if !ok || !validTransferTracker(tracker) {
				return nil, ErrInvalid
			}
			count++
			if count > 128 {
				return nil, ErrInvalid
			}
			tier = append(tier, tracker)
		}
		if len(tier) > 0 {
			tiers = append(tiers, tier)
		}
	}
	return tiers, nil
}
func uniqueTrackers(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		if !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	return out
}
func transferString(value string) []byte { return []byte(strconv.Itoa(len(value)) + ":" + value) }
func encodeTransferTiers(tiers [][]string) []byte {
	out := []byte{'l'}
	for _, tier := range tiers {
		out = append(out, 'l')
		for _, tracker := range tier {
			out = append(out, transferString(tracker)...)
		}
		out = append(out, 'e')
	}
	return append(out, 'e')
}
func transferTopValues(data []byte) (map[string][]byte, error) {
	if len(data) < 2 || data[0] != 'd' {
		return nil, ErrInvalid
	}
	pos := 1
	fields := map[string][]byte{}
	for pos < len(data) && data[pos] != 'e' {
		key, ok := readString(data, &pos)
		if !ok {
			return nil, ErrInvalid
		}
		start := pos
		if !readValue(data, &pos, 1) {
			return nil, ErrInvalid
		}
		if _, exists := fields[string(key)]; exists {
			return nil, ErrInvalid
		}
		fields[string(key)] = bytes.Clone(data[start:pos])
	}
	if pos != len(data)-1 || data[pos] != 'e' {
		return nil, ErrInvalid
	}
	return fields, nil
}
func patchTransferRoot(original, changes map[string][]byte) ([]byte, error) {
	fields := map[string][]byte{}
	for key, value := range original {
		fields[key] = value
	}
	for key, value := range changes {
		if key != "announce" && key != "announce-list" {
			return nil, ErrInvalid
		}
		fields[key] = value
	}
	keys := []string{}
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []byte{'d'}
	for _, key := range keys {
		out = append(out, transferString(key)...)
		out = append(out, fields[key]...)
		if len(out) > 8<<20 {
			return nil, ErrInvalid
		}
	}
	return append(out, 'e'), nil
}
func mustTransferInfo(data []byte) []byte {
	fields, err := transferTopValues(data)
	if err != nil {
		return nil
	}
	return fields["info"]
}
func sameTransferFiles(a, b []torrentfiles.File) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
