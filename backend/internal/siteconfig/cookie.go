package siteconfig

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// CookieNote changes only ua. Raw values retain unknown fields and integers
// beyond JavaScript/float64 precision, including the old site's extra options.
func CookieNote(raw, agent string) (string, error) {
	values := map[string]json.RawMessage{}
	if raw != "" && raw != "null" {
		d := json.NewDecoder(strings.NewReader(raw))
		token, err := d.Token()
		if err != nil || token != json.Delim('{') {
			return "", errors.New("invalid site attributes")
		}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return "", err
			}
			name, ok := key.(string)
			if !ok {
				return "", errors.New("invalid site attribute key")
			}
			if _, exists := values[name]; exists {
				return "", errors.New("ambiguous site attribute key")
			}
			var value json.RawMessage
			if err = d.Decode(&value); err != nil {
				return "", err
			}
			values[name] = value
		}
		if _, err = d.Token(); err != nil {
			return "", err
		}
		if _, err = d.Token(); !errors.Is(err, io.EOF) {
			return "", errors.New("trailing site attributes")
		}
	}
	if agent != "" {
		values["ua"], _ = json.Marshal(agent)
	}
	result, err := json.Marshal(values)
	return string(result), err
}
