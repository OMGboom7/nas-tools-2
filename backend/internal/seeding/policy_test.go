package seeding

import (
	"math"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestEvaluateRequiresEveryObligationAndExactFreshCounters(t *testing.T) {
	hash := strings.Repeat("a", 40)
	r := Requirements{Seconds: ptr(int64(86400)), Ratio: ptr(1.5), HR: ptr(false)}
	e := Evidence{Hash: hash, Complete: ptr(true), Seconds: ptr(int64(86400)), Ratio: ptr(1.5)}
	if d := Evaluate(strings.ToUpper(hash), []Requirements{r}, e); !d.Allowed || len(d.Reasons) != 0 {
		t.Fatal(d)
	}
	for _, test := range []struct {
		name   string
		change func(*Requirements, *Evidence)
	}{
		{"wrong hash", func(r *Requirements, e *Evidence) { e.Hash = strings.Repeat("b", 40) }},
		{"unknown completion", func(r *Requirements, e *Evidence) { e.Complete = nil }},
		{"incomplete", func(r *Requirements, e *Evidence) { e.Complete = ptr(false) }},
		{"unknown HR", func(r *Requirements, e *Evidence) { r.HR = nil }},
		{"HR never cleared by counters", func(r *Requirements, e *Evidence) { r.HR = ptr(true) }},
		{"unknown minimum seconds", func(r *Requirements, e *Evidence) { r.Seconds = nil }},
		{"unknown minimum ratio", func(r *Requirements, e *Evidence) { r.Ratio = nil }},
		{"unknown seconds", func(r *Requirements, e *Evidence) { e.Seconds = nil }},
		{"unknown ratio", func(r *Requirements, e *Evidence) { e.Ratio = nil }},
		{"insufficient seconds", func(r *Requirements, e *Evidence) { e.Seconds = ptr(int64(86399)) }},
		{"insufficient ratio", func(r *Requirements, e *Evidence) { e.Ratio = ptr(1.49) }},
		{"negative sentinel seconds", func(r *Requirements, e *Evidence) { e.Seconds = ptr(int64(-1)) }},
		{"negative sentinel ratio", func(r *Requirements, e *Evidence) { e.Ratio = ptr(-1.0) }},
		{"nonfinite ratio", func(r *Requirements, e *Evidence) { e.Ratio = ptr(math.NaN()) }},
		{"invalid requirement", func(r *Requirements, e *Evidence) { r.Ratio = ptr(math.Inf(1)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			rr, ee := r, e
			test.change(&rr, &ee)
			d := Evaluate(hash, []Requirements{r, rr}, ee)
			if d.Allowed || len(d.Reasons) == 0 {
				t.Fatal(d)
			}
		})
	}
	zero := Requirements{Seconds: ptr(int64(0)), Ratio: ptr(0.0), HR: ptr(false)}
	if Evaluate(hash, []Requirements{zero}, Evidence{Hash: hash, Complete: ptr(true)}).Allowed {
		t.Fatal("missing counters became zero")
	}
	if !Evaluate(hash, nil, Evidence{}).Allowed {
		t.Fatal("unregistered compatibility changed")
	}
}

func TestHashAndRequirementBounds(t *testing.T) {
	for _, h := range []string{"", strings.Repeat("a", 39), strings.Repeat("g", 40), strings.Repeat("a", 65)} {
		if ValidHash(h) {
			t.Fatal(h)
		}
	}
	for _, n := range []int{40, 64} {
		if !ValidHash(strings.Repeat("A", n)) {
			t.Fatal(n)
		}
	}
	if (Requirements{Seconds: ptr(int64(-1))}).Valid() || (Requirements{Ratio: ptr(-1.0)}).Valid() || (Requirements{Seconds: ptr(int64(315360001))}).Valid() {
		t.Fatal("invalid requirements")
	}
}
