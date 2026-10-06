package torrentmeta

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestParseHashesExactInfoDictionary(t *testing.T) {
	info := "d4:name4:Teste"
	data := []byte("d8:announce16:https://site.tld4:info" + info + "e")
	metadata, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want := sha1.Sum([]byte(info))
	if metadata.Name != "Test" || metadata.InfoHash != hex.EncodeToString(want[:]) {
		t.Fatalf("metadata = %#v", metadata)
	}
	withDifferentAnnounce := []byte("d8:announce16:https://else.tld4:info" + info + "e")
	other, err := Parse(withDifferentAnnounce)
	if err != nil || other.InfoHash != metadata.InfoHash {
		t.Fatalf("tracker changed info hash: %#v %v", other, err)
	}
}

func TestParseV2UsesSHA256(t *testing.T) {
	data := []byte("d4:infod12:meta versioni2e4:name4:Testee")
	metadata, err := Parse(data)
	if err != nil || metadata.Name != "Test" || len(metadata.InfoHash) != 64 {
		t.Fatalf("v2 = %#v %v", metadata, err)
	}
}

func TestParseRejectsMalformedMetainfo(t *testing.T) {
	for _, input := range []string{"", "de", "d4:infole", "d4:infod4:name4:Testeejunk", "d4:infod4:name999:Teste", "d4:infod4:name4:Testee4:infole", "d4:infod4:name4:Testi01eee", "d4:infod4:name4:Testee" + strings.Repeat("x", (8<<20)+1)} {
		if _, err := Parse([]byte(input)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted %q: %v", input[:min(len(input), 60)], err)
		}
	}
}
