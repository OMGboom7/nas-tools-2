package torrentmeta

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"net/url"
	"strings"
)

// DownloadLayout returns the containing folder and relative payload filenames
// used by classic episode selection and subtitle/history lookup.
func DownloadLayout(raw []byte) (string, []string, error) {
	meta, manifest, err := Manifest(raw)
	if err != nil {
		return "", nil, err
	}
	folder := ""
	names := make([]string, 0, len(manifest))
	for _, f := range manifest {
		if f.Padding {
			continue
		}
		name := f.Path
		if len(meta.InfoHash) == 64 && len(manifest) == 1 && name != meta.Name {
			// Manifest retains the raw tree path for a pure v2 tree with one leaf.
			folder = meta.Name
		} else if strings.HasPrefix(name, meta.Name+"/") {
			folder = meta.Name
			name = strings.TrimPrefix(name, folder+"/")
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "", nil, ErrInvalid
	}
	return folder, names, nil
}

func MagnetLink(raw []byte) (string, error) {
	m, err := PrepareTransfer(raw, nil)
	if err != nil {
		return "", err
	}
	var topics []string
	if m.V1Hash != "" {
		topics = append(topics, "xt=urn:btih:"+m.V1Hash)
	}
	if m.V2Hash != "" {
		topics = append(topics, "xt=urn:btmh:1220"+m.V2Hash)
	}
	params := url.Values{"dn": {m.Name}, "tr": m.TrackerURLs}
	return "magnet:?" + strings.Join(topics, "&") + "&" + params.Encode(), nil
}

func v1TopicHash(topic string) string {
	topic = strings.ToLower(topic)
	if !strings.HasPrefix(topic, "urn:btih:") {
		return ""
	}
	hash := strings.TrimPrefix(topic, "urn:btih:")
	switch len(hash) {
	case 40:
		if _, err := hex.DecodeString(hash); err == nil {
			return hash
		}
	case 32:
		b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(hash))
		if err == nil && len(b) == 20 {
			return hex.EncodeToString(b)
		}
	}
	return ""
}

func MagnetInfoHash(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	v1, v2, err := MagnetTopics(u.Query()["xt"])
	if err != nil {
		return ""
	}
	if v1 != "" {
		return v1
	}
	return v2
}

// MagnetTopics checks conflicting v1/v2 identities and normalizes base32 btih.
func MagnetTopics(topics []string) (string, string, error) {
	var v1, v2 string
	for _, topic := range topics {
		topic = strings.ToLower(topic)
		switch {
		case strings.HasPrefix(topic, "urn:btih:"):
			h := v1TopicHash(topic)
			if h == "" || v1 != "" && v1 != h {
				return "", "", ErrInvalid
			}
			v1 = h
		case strings.HasPrefix(topic, "urn:btmh:"):
			h, ok := strings.CutPrefix(topic, "urn:btmh:1220")
			if _, err := hex.DecodeString(h); !ok || err != nil || len(h) != 64 || v2 != "" && v2 != h {
				return "", "", ErrInvalid
			}
			v2 = h
		}
	}
	return v1, v2, nil
}

// ResolveLink checks every advertised identity before returning metadata.
func ResolveLink(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(raw, "magnet:?") || strings.ContainsAny(raw, " \t\r\n\x00") {
		return nil, ErrInvalid
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, ErrInvalid
	}
	v1, v2, err := MagnetTopics(q["xt"])
	if err != nil {
		return nil, err
	}
	rawTorrent, err := ResolveMagnet(ctx, raw, v1)
	if err != nil {
		return nil, err
	}
	if v2 != "" {
		m, err := PrepareTransfer(rawTorrent, nil)
		if err != nil || m.V2Hash != v2 {
			return nil, ErrInvalid
		}
	}
	return rawTorrent, nil
}
