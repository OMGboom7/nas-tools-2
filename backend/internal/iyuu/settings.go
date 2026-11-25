package iyuu

import (
	"encoding/json"
	"slices"
	"strings"
)

type Settings struct {
	Token                        string
	Downloaders, Sites, Excluded []string
	Success, Failed, Permanent   map[string]bool
}

func Values(raw string) (map[string]json.RawMessage, error) {
	v := map[string]json.RawMessage{}
	if raw != "" && (json.Unmarshal([]byte(raw), &v) != nil || v == nil) {
		return nil, ErrInput
	}
	return v, nil
}

func Read(raw string) (Settings, error) {
	var s Settings
	v, err := Values(raw)
	if err != nil {
		return s, err
	}
	for k, dst := range map[string]*[]string{"downloaders": &s.Downloaders, "sites": &s.Sites} {
		var ids []json.Number
		if b := v[k]; b != nil && json.Unmarshal(b, &ids) != nil {
			return s, ErrInput
		}
		for _, id := range ids {
			if !positiveID(string(id)) {
				return s, ErrInput
			}
			if !slices.Contains(*dst, string(id)) {
				*dst = append(*dst, string(id))
			}
		}
	}
	if b := v["token"]; b != nil && (json.Unmarshal(b, &s.Token) != nil || s.Token != "" && !ValidToken(s.Token)) {
		return s, ErrInput
	}
	var labels string
	if b := v["nolabels"]; b != nil && json.Unmarshal(b, &labels) != nil {
		return s, ErrInput
	}
	for _, v := range strings.Split(labels, ",") {
		if v = strings.TrimSpace(v); v != "" {
			s.Excluded = append(s.Excluded, v)
		}
	}
	for k, dst := range map[string]*map[string]bool{"error_caches": &s.Failed, "success_caches": &s.Success, "permanent_error_caches": &s.Permanent} {
		var list []string
		if b := v[k]; b != nil && json.Unmarshal(b, &list) != nil {
			return s, ErrInput
		}
		m := make(map[string]bool, len(list))
		for _, h := range list {
			m[strings.ToLower(h)] = true
		}
		*dst = m
	}
	return s, nil
}
