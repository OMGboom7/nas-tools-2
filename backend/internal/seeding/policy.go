package seeding

import (
	"math"
	"strings"
)

type Requirements struct {
	Seconds *int64   `json:"seconds"`
	Ratio   *float64 `json:"ratio"`
	HR      *bool    `json:"hr"`
}
type Evidence struct {
	Hash     string   `json:"hash"`
	State    string   `json:"state"`
	Complete *bool    `json:"complete"`
	Seconds  *int64   `json:"seconds"`
	Ratio    *float64 `json:"ratio"`
}
type Decision struct {
	Allowed bool     `json:"allowed"`
	Reasons []string `json:"reasons"`
}

func ValidHash(hash string) bool {
	if len(hash) != 40 && len(hash) != 64 {
		return false
	}
	for _, c := range hash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
func (r Requirements) Valid() bool {
	return (r.Seconds == nil || *r.Seconds >= 0 && *r.Seconds <= 315360000) && (r.Ratio == nil || !math.IsNaN(*r.Ratio) && !math.IsInf(*r.Ratio, 0) && *r.Ratio >= 0 && *r.Ratio <= 1000000)
}
func Evaluate(hash string, rules []Requirements, e Evidence) Decision {
	d := Decision{Allowed: true, Reasons: []string{}}
	deny := func(s string) {
		d.Allowed = false
		for _, old := range d.Reasons {
			if old == s {
				return
			}
		}
		d.Reasons = append(d.Reasons, s)
	}
	if len(rules) == 0 {
		return d
	}
	if !ValidHash(hash) || !strings.EqualFold(hash, e.Hash) {
		deny("task identity unavailable")
		return d
	}
	if e.Complete == nil {
		deny("completion is unknown")
	} else if !*e.Complete {
		deny("task is not completely downloaded")
	}
	for _, r := range rules {
		if !r.Valid() {
			deny("stored requirements are invalid")
			continue
		}
		if r.HR == nil {
			deny("HR status is unknown")
		} else if *r.HR {
			deny("HR marker is recorded; tracker-side clearance is not verified")
		}
		if r.Seconds == nil {
			deny("minimum seed time is unknown")
		} else if e.Seconds == nil || *e.Seconds < 0 {
			deny("reported seed time is unknown")
		} else if *e.Seconds < *r.Seconds {
			deny("minimum seed time has not been reached")
		}
		if r.Ratio == nil {
			deny("minimum ratio is unknown")
		} else if e.Ratio == nil || math.IsNaN(*e.Ratio) || math.IsInf(*e.Ratio, 0) || *e.Ratio < 0 {
			deny("reported ratio is unknown")
		} else if *e.Ratio < *r.Ratio {
			deny("minimum ratio has not been reached")
		}
	}
	return d
}
