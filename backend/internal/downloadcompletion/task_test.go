package downloadcompletion

import (
	"strings"
	"testing"
)

func validTask() Task {
	return Task{ID: strings.Repeat("A", 40), Name: "display name is not a path", Directory: "/downloads/", State: "complete", Complete: true, LabelsKnown: true, Labels: []string{"用户", "NASTOOL"}, Files: []File{{Path: "Bundle/film.mkv", Size: 14, Selected: true, Complete: true}, {Path: "unwanted.mkv", Size: 99, Selected: false}}}
}
func TestManifestNormalizesWithoutGuessingOwnershipOrUnwantedCompletion(t *testing.T) {
	v, err := Normalize(validTask())
	if err != nil || !v.Complete || v.Directory != "/downloads" || v.ID != strings.Repeat("a", 40) || !v.HasLabel("NASTOOL") || v.HasLabel("NAST") {
		t.Fatal(v, err)
	}
	v.LabelsKnown = false
	if v.HasLabel("NASTOOL") {
		t.Fatal("invented labels")
	}
}
func TestManifestRejectsUnsafeAmbiguousOrUnboundedEvidence(t *testing.T) {
	for _, name := range []string{"absolute", "parent", "dot", "backslash", "duplicate", "size", "directory", "labels", "limit", "nul"} {
		t.Run(name, func(t *testing.T) {
			v := validTask()
			switch name {
			case "absolute":
				v.Files[0].Path = "/elsewhere/file"
			case "parent":
				v.Files[0].Path = "../file"
			case "dot":
				v.Files[0].Path = "dir/../file"
			case "backslash":
				v.Files[0].Path = `dir\file`
			case "duplicate":
				v.Files = append(v.Files, v.Files[0])
			case "size":
				v.Files[0].Size = -1
			case "directory":
				v.Directory = "/"
			case "labels":
				v.Labels = []string{"NASTOOL", "NASTOOL"}
			case "limit":
				v.Files = make([]File, 1001)
			case "nul":
				v.Files[0].Path = "bad\x00name"
			}
			if _, err := Normalize(v); err == nil {
				t.Fatal(v)
			}
		})
	}
	v := validTask()
	v.Files[0].Complete = false
	r, err := Normalize(v)
	if err != nil || r.Complete {
		t.Fatal(r, err)
	}
	v.Files[0].Selected = false
	r, err = Normalize(v)
	if err != nil || r.Complete {
		t.Fatal("no selected files", r, err)
	}
}
func TestMappingUsesLongestComponentPrefixWithoutStringReplacement(t *testing.T) {
	raw := `[{"save_path":"/remote","container_path":"/local"},{"save_path":"/remote/nested","container_path":"/specific"}]`
	got, err := Resolve(raw, "/remote/nested/remote", true)
	if err != nil || got.Root != "/specific" || got.Directory != "/specific/remote" || !got.Mapped {
		t.Fatal(got, err)
	}
	got, err = Resolve(raw, "/local/sub", false)
	if err != nil || got.Directory != "/local/sub" || got.Mapped {
		t.Fatal(got, err)
	}
	if _, err := Resolve(raw, "/local/sub", true); err == nil {
		t.Fatal("match_path bypass")
	}
	for _, s := range []string{"/remotely/file", "/unconfigured", "/remote/../escape", `C:\Downloads`} {
		if _, err := Resolve(raw, s, false); err == nil {
			t.Fatal(s)
		}
	}
}
func TestMappingRejectsEqualSpecificityDisagreementAndMalformedRows(t *testing.T) {
	for _, raw := range []string{`[]`, `null`, `{}`, `[{"save_path":"/r","container_path":"/a"},{"save_path":"/r","container_path":"/b"}]`, `[{"save_path":"/r","container_path":"/"}]`, `[{"save_path":"/r/../a"}]`} {
		if _, err := Resolve(raw, "/r", false); err == nil {
			t.Fatal(raw)
		}
	}
	v, err := Resolve(`[{"save_path":"/r/"}]`, "/r/", true)
	if err != nil || v.Directory != "/r" {
		t.Fatal(v, err)
	}
}
func TestCompletionIDValidation(t *testing.T) {
	for _, k := range []string{"qbittorrent", "transmission", "aria2"} {
		n := 40
		if k == "aria2" {
			n = 16
		}
		if !ValidID(k, strings.Repeat("A", n)) || ValidID(k, strings.Repeat("x", n)) || ValidID(k, "") {
			t.Fatal(k)
		}
	}
	if !ValidID("qbittorrent", strings.Repeat("f", 64)) || ValidID("transmission", strings.Repeat("f", 64)) || ValidID("unknown", strings.Repeat("f", 40)) {
		t.Fatal("ID width")
	}
}

func TestOrganizedMarkerHasReservedLabelBudget(t *testing.T) {
	v := validTask()
	v.Labels = nil
	for i := 0; i < 64; i++ {
		v.Labels = append(v.Labels, strings.Repeat("x", i+1))
	}
	if _, err := Normalize(v); err != nil {
		t.Fatal(err)
	}
	v.Labels = append(v.Labels, "已整理")
	if _, err := Normalize(v); err != nil {
		t.Fatal("cannot verify just-written marker", err)
	}
	v.Labels[64] = "extra-user-label"
	if _, err := Normalize(v); err == nil {
		t.Fatal("label bound bypass")
	}
}
