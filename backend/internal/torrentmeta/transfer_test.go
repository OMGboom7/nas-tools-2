package torrentmeta

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"strings"
	"testing"
)

func transferFixture(private bool, announce string) ([]byte, []byte) {
	info := []byte("d6:lengthi8e4:name9:movie.mkv")
	if private {
		info = append(info, []byte("7:privatei1e")...)
	}
	info = append(info, 'e')
	root := map[string][]byte{"comment": transferString("original-private-field"), "info": info}
	if announce != "" {
		root["announce"] = transferString(announce)
	}
	data, _ := patchTransferRoot(root, nil)
	return data, info
}
func transferResume(info []byte, urls ...string) []byte {
	hash := sha1.Sum(info)
	out := []byte("d9:info-hash20:")
	out = append(out, hash[:]...)
	out = append(out, []byte("8:trackers")...)
	tiers := [][]string{}
	for _, u := range urls {
		tiers = append(tiers, []string{u})
	}
	out = append(out, encodeTransferTiers(tiers)...)
	return append(out, 'e')
}
func TestTransferTrackerRepairPreservesExactInfoAndPrivateOuterValues(t *testing.T) {
	data, info := transferFixture(true, "")
	urls := []string{"https://tracker.fixture.test/announce?passkey=fixture-private", "udp://tracker.fixture.test:80/announce"}
	got, err := PrepareTransfer(data, transferResume(info, urls...))
	if err != nil {
		t.Fatal(err)
	}
	original, _ := Parse(data)
	if !got.Patched || !got.Private || got.InfoHash != original.InfoHash || got.TrackerCount != 2 || !bytes.Equal(mustTransferInfo(got.Content), info) {
		t.Fatal(got.InfoHash, got.TrackerCount)
	}
	fields, _ := transferTopValues(got.Content)
	if !bytes.Equal(fields["comment"], transferString("original-private-field")) {
		t.Fatal("unknown root value changed")
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "fixture-private") || strings.Contains(string(raw), "original-private-field") {
		t.Fatal("metainfo escaped into public progress")
	}
	if !bytes.Equal(mustTransferInfo(data), info) {
		t.Fatal("input overwritten")
	}
}
func TestTransferExistingTrackerNeverUsesOrRewritesForeignResume(t *testing.T) {
	data, _ := transferFixture(true, "https://tracker.fixture.test/a?passkey=original")
	got, err := PrepareTransfer(data, []byte("not a resume"))
	if err != nil || got.Patched || !bytes.Equal(got.Content, data) || got.TrackerCount != 1 {
		t.Fatal(err, got.Patched)
	}
}
func TestTransferRefusesForeignResumeAndUnknownPrivateTracker(t *testing.T) {
	data, info := transferFixture(true, "")
	if _, err := PrepareTransfer(data, nil); err == nil {
		t.Fatal("private tracker guessed")
	}
	resume := transferResume(info, "https://tracker.fixture.test/a")
	index := bytes.Index(resume, []byte("20:")) + 3
	resume[index] ^= 1
	if _, err := PrepareTransfer(data, resume); err == nil {
		t.Fatal("foreign resume accepted")
	}
	for _, bad := range []string{"file:///private/fixture", "javascript:fixture", "https://fixture.test/a#fragment", "https://fixture.test/a\nheader"} {
		if _, err := PrepareTransfer(data, transferResume(info, bad)); err == nil {
			t.Fatal(bad)
		}
	}
}
func TestTransferPublicDHTMetadataRemainsUnmodified(t *testing.T) {
	data, _ := transferFixture(false, "")
	got, err := PrepareTransfer(data, nil)
	if err != nil || got.Private || got.Patched || got.TrackerCount != 0 || !bytes.Equal(got.Content, data) {
		t.Fatal(err, got.Patched, got.TrackerCount)
	}
}
func TestTransferPromotesItsExistingAnnounceTierWithoutResume(t *testing.T) {
	data, info := transferFixture(true, "")
	fields, _ := transferTopValues(data)
	fields["announce-list"] = encodeTransferTiers([][]string{{"https://tracker.fixture.test/original"}})
	data, _ = patchTransferRoot(fields, nil)
	got, err := PrepareTransfer(data, nil)
	if err != nil || !got.Patched || got.TrackerCount != 1 || !bytes.Equal(mustTransferInfo(got.Content), info) {
		t.Fatal(err, got.Patched)
	}
}

func TestTransferRepairPreservesV2AndHybridInfoBytes(t *testing.T) {
	for _, hybrid := range []bool{false, true} {
		info := map[string]any{"name": "Fixture", "meta version": 2, "file tree": map[string]any{"movie.mkv": map[string]any{"": map[string]any{"length": 8}}}, "private": 1}
		if hybrid {
			info["pieces"] = strings.Repeat("x", 20)
			info["files"] = []any{map[string]any{"length": 8, "path": []any{"movie.mkv"}}}
		}
		data := manifestMetainfo(info)
		rawInfo := mustTransferInfo(data)
		got, err := PrepareTransfer(data, transferResume(rawInfo, "https://tracker.fixture.test/original"))
		want, _ := Parse(data)
		if err != nil || got.InfoHash != want.InfoHash || !bytes.Equal(mustTransferInfo(got.Content), rawInfo) {
			t.Fatal(hybrid, err, got.InfoHash, want.InfoHash)
		}
	}
}
