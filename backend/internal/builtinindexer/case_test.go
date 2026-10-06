package builtinindexer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func TestNumericCasesPreserveOrderAndUnknown(t *testing.T) {
	ctx := context.Background()
	doc, err := ParseDocument(ctx, []byte(`<div><img class="free"><img class="half"><span class="factor">0.5倍</span></div>`))
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		raw   string
		want  float64
		known bool
	}{
		{`{".half":0.5,".free":0,"*":1}`, 0.5, true},
		{`{".free":0,".half":0.5,"*":1}`, 0, true},
		{`{"*":1,".free":0}`, 1, true},
		{`{".missing":0,"*":1}`, 1, true},
		{`{".missing":0}`, 0, false},
		{`{"img:not(.free):has(a)":0,".half":0.3}`, 0.3, true},
	} {
		value, err := NumericCaseValue(ctx, doc.Root, json.RawMessage(fixture.raw))
		if err != nil || (value != nil) != fixture.known || (value != nil && *value != fixture.want) {
			t.Fatalf("%s => %v error=%v", fixture.raw, value, err)
		}
	}
	value, err := NumericFieldValue(ctx, doc.Root, FieldSpec{Selector: ".factor"})
	if err != nil || value == nil || *value != 0.5 {
		t.Fatal(value, err)
	}
	value, err = NumericFieldValue(ctx, doc.Root, FieldSpec{Selector: ".missing"})
	if err != nil || value != nil {
		t.Fatal(value, err)
	}
	value, err = NumericFieldValue(ctx, doc.Root, FieldSpec{})
	if err != nil || value != nil {
		t.Fatal(value, err)
	}
}

func TestNumericCasesRejectMalformedRules(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{}`, `{"*":null}`, `{"*":"0"}`, `{"*":-1}`, `{"*":1e1000}`, `{"*":1000000001}`, `{"*":1,"*":0}`, `{"*":1} {}`, `{"[":0}`, `{"*":1,"[":0}`} {
		if _, err := compileNumericCases(json.RawMessage(raw)); !errors.Is(err, ErrConfig) {
			t.Fatal(raw, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NumericCaseValue(ctx, nil, json.RawMessage(`{"*":1}`)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := NumericFieldValue(ctx, nil, FieldSpec{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := NumericCaseValue(context.Background(), nil, json.RawMessage(`{"*":1}`)); !errors.Is(err, ErrResponse) {
		t.Fatal(err)
	}
}

func TestNumericFieldValues(t *testing.T) {
	for _, fixture := range []struct {
		text string
		want float64
		bad  bool
	}{
		{".5倍", 0.5, false}, {"2倍", 2, false}, {"0", 0, false}, {"1.0", 1, false},
		{"-0.5倍", 0, true}, {"1.2.3", 0, true}, {"1e3", 0, true}, {"unknown", 0, true},
	} {
		doc, err := ParseDocument(context.Background(), []byte("<span>"+fixture.text+"</span>"))
		if err != nil {
			t.Fatal(err)
		}
		value, err := NumericFieldValue(context.Background(), doc.Root, FieldSpec{Selector: "span"})
		if fixture.bad {
			if !errors.Is(err, ErrResponse) {
				t.Fatal(fixture, err)
			}
			continue
		}
		if err != nil || value == nil || *value != fixture.want {
			t.Fatal(fixture, value, err)
		}
	}
}

func TestBundledNumericCasesCompile(t *testing.T) {
	catalog, err := indexercatalog.Load("../../../web/backend/user.sites.bin")
	if err != nil {
		t.Fatal(err)
	}
	count, invalid := 0, 0
	for _, definition := range catalog.Indexers {
		var rules struct {
			Fields map[string]FieldSpec `json:"fields"`
		}
		if len(definition.Torrents) == 0 {
			continue
		}
		if err := json.Unmarshal(definition.Torrents, &rules); err != nil {
			t.Fatal(err)
		}
		for field, rule := range rules.Fields {
			if len(rule.Case) == 0 {
				continue
			}
			count++
			if _, err := compileNumericCases(rule.Case); err != nil {
				// Existing kufei fallback keys are empty CSS, not '*'. Keep them
				// explicit errors instead of silently manufacturing a match.
				if definition.ID == "kufei" && (field == "uploadvolumefactor" || field == "downloadvolumefactor") && errors.Is(err, ErrConfig) {
					invalid++
				} else {
					t.Errorf("%s %s: %v", definition.ID, field, err)
				}
			}
		}
	}
	if count != 232 || invalid != 2 {
		t.Fatal(count, invalid)
	}
	t.Logf("compiled %d ordered numeric case definitions; rejected %d empty-selector definitions", count-invalid, invalid)
}
