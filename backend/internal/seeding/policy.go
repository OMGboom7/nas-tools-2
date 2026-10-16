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
	AddedAt  *int64   `json:"addedAt"`
	Hash     string   `json:"hash"`
	State    string   `json:"state"`
	Complete *bool    `json:"complete"`
	Seconds  *int64   `json:"seconds"`
	Ratio    *float64 `json:"ratio"`
}

// An operator attestation is NOT automatic tracker evidence. Callers must
// independently validate its actor, exact policy set, scope and short expiry.
// Known minima cannot be waived, and a reset/re-added task invalidates it.
func EvaluateAttested(hash string, rules []Requirements, current, observed Evidence) Decision {
	deny := func(reason string) Decision { return Decision{Allowed: false, Reasons: []string{reason}} }
	if !ValidHash(hash) || !strings.EqualFold(current.Hash, hash) || !strings.EqualFold(observed.Hash, hash) || current.AddedAt == nil || observed.AddedAt == nil || *current.AddedAt < 1 || *current.AddedAt != *observed.AddedAt {
		return deny("attested task identity changed or is unknown")
	}
	if current.Complete == nil || !*current.Complete || observed.Complete == nil || !*observed.Complete {
		return deny("attested task completion is unknown or changed")
	}
	valid := func(e Evidence) bool {
		return e.Seconds != nil && *e.Seconds >= 0 && e.Ratio != nil && *e.Ratio >= 0 && !math.IsNaN(*e.Ratio) && !math.IsInf(*e.Ratio, 0)
	}
	if !valid(current) || !valid(observed) {
		return deny("attested counters are unknown")
	}
	if *current.Seconds < *observed.Seconds || *current.Ratio < *observed.Ratio {
		return deny("attested task counters regressed")
	}
	for _, r := range rules {
		if !r.Valid() {
			return deny("stored requirements are invalid")
		}
		if r.Seconds != nil && *current.Seconds < *r.Seconds {
			return deny("known minimum seed time has not been reached")
		}
		if r.Ratio != nil && *current.Ratio < *r.Ratio {
			return deny("known minimum ratio has not been reached")
		}
	}
	return Decision{Allowed: true, Reasons: []string{}}
}

type Decision struct {
	Basis   string   `json:"basis,omitempty"`
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
