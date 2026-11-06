package torrentmark

import (
	"slices"
	"testing"
)

func TestClassificationAndPreservingUnrelatedLabels(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name     string
		private  *bool
		trackers []string
		labels   []string
		want     string
	}{
		{"legacy private", nil, []string{"https://tracker.test/announce?passkey=fixture"}, nil, PT},
		{"private metadata", &yes, []string{"udp://tracker.test:80/announce"}, nil, PT},
		{"public metadata", &no, []string{"https://tracker.test/announce"}, []string{PT}, BT},
		{"public legacy", nil, []string{"udp://tracker.test:80/announce", "https://other.test/a"}, nil, BT},
		{"private protected", nil, []string{"udp://tracker.test:80/announce"}, []string{PT}, ""},
		{"conflicting", &no, []string{"https://tracker.test/a?passkey=fixture"}, nil, ""},
		{"empty", &yes, nil, nil, ""}, {"malformed", &yes, []string{"not a tracker"}, nil, ""},
		{"duplicate", nil, []string{"https://tracker.test/a", "https://tracker.test/a"}, nil, ""},
		{"multiple private cues", nil, []string{"https://tracker.test/a?passkey=fixture", "https://other.test/a"}, nil, ""},
		{"credentials", &yes, []string{"https://user:pass@tracker.test/a"}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Evidence{Added: 1, Complete: true, Private: tc.private, Trackers: tc.trackers, Labels: tc.labels}
			if got := Classify(e); got != tc.want {
				t.Fatal(got, tc.want)
			}
			e.Complete = false
			if Classify(e) != "" {
				t.Fatal("incomplete accepted")
			}
		})
	}
	got := LabelsAfter([]string{"keep", BT, "已整理"}, PT)
	if !slices.Equal(got, []string{PT, "keep", "已整理"}) {
		t.Fatal(got)
	}
	for _, labels := range [][]string{nil, {"a", "a"}, {""}, {" bad"}, {"line\nbreak"}} {
		if _, err := NormalizeLabels(labels); err == nil {
			t.Fatal(labels)
		}
	}
}
