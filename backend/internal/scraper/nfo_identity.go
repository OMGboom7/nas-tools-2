package scraper

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
)

// NFOIdentity accepts only an explicit TMDB identifier in the expected root.
// It never fetches external entities or treats an IMDb/default id as a TMDB id.
func NFOIdentity(data []byte, kind string) (string, error) {
	root := "movie"
	if kind == "tv" {
		root = "tvshow"
	} else if kind != "movie" {
		return "", ErrMetadata
	}
	if len(data) == 0 || len(data) > 2<<20 {
		return "", ErrMetadata
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	seen := false
	closed := false
	id := ""
	for {
		t, err := d.Token()
		if err == io.EOF {
			if seen && closed {
				return id, nil
			}
			return "", ErrMetadata
		}
		if err != nil {
			return "", ErrMetadata
		}
		switch v := t.(type) {
		case xml.Directive:
			return "", ErrMetadata
		case xml.StartElement:
			depth++
			if depth > 64 || closed || v.Name.Space != "" {
				return "", ErrMetadata
			}
			if depth == 1 {
				if seen || v.Name.Local != root {
					return "", ErrMetadata
				}
				seen = true
				continue
			}
			want := depth == 2 && v.Name.Local == "tmdbid"
			if depth == 2 && v.Name.Local == "uniqueid" {
				types := 0
				for _, a := range v.Attr {
					if a.Name.Local == "type" {
						types++
					}
					if a.Name.Local == "type" && a.Value == "tmdb" {
						want = true
					}
				}
				if types > 1 {
					return "", ErrMetadata
				}
			}
			if want {
				s := ""
				for {
					token, err := d.Token()
					if err != nil {
						return "", ErrMetadata
					}
					if _, ok := token.(xml.EndElement); ok {
						break
					}
					if c, ok := token.(xml.CharData); ok {
						s += string(c)
						if len(s) > 128 {
							return "", ErrMetadata
						}
					} else if _, ok := token.(xml.Comment); !ok {
						return "", ErrMetadata
					}
				}
				depth--
				s = strings.TrimSpace(s)
				if !canonicalID(s) || id != "" && id != s {
					return "", ErrMetadata
				}
				id = s
			}
		case xml.EndElement:
			depth--
			if depth == 0 {
				closed = true
			}
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(v)) != "" {
				return "", ErrMetadata
			}
		}
	}
}
