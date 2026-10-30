package sitelogin

import "encoding/json"

type Credentials struct{ Username, Password, TwoStep string }

func ParseCredentials(raw string) (Credentials, error) {
	out := Credentials{}
	if raw == "" || raw == "null" {
		return out, nil
	}
	var values map[string]json.RawMessage
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &values) != nil || values == nil {
		return out, ErrScope
	}
	for key, target := range map[string]*string{"username": &out.Username, "password": &out.Password, "two_step_code": &out.TwoStep} {
		if v, exists := values[key]; exists && string(v) != "null" {
			if json.Unmarshal(v, target) != nil {
				return out, ErrScope
			}
		}
	}
	if !Text(out.Username, 1024) || !Text(out.Password, 4096) || !Text(out.TwoStep, 128) {
		return out, ErrScope
	}
	return out, nil
}
