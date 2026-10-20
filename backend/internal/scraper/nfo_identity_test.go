package scraper

import (
	"strings"
	"testing"
)

func TestNFOIdentitySelectsOnlyConsistentExplicitTMDBIDs(t *testing.T) {
	for _, tc := range []struct{ xml, kind, id string }{
		{`<?xml version="1.0"?><movie><uniqueid type="imdb">tt100</uniqueid><uniqueid type="tmdb">123</uniqueid><tmdbid>123</tmdbid></movie>`, "movie", "123"},
		{`<tvshow><tmdbid>200</tmdbid></tvshow>`, "tv", "200"},
		{`<movie><uniqueid type="Tmdb">101</uniqueid><uniqueid type="TMDB">101</uniqueid></movie>`, "movie", "101"},
		{`<movie><id>100</id><uniqueid type="imdb">tt100</uniqueid></movie>`, "movie", ""},
		{`<movie><actor><tmdbid>400</tmdbid></actor></movie>`, "movie", ""},
	} {
		id, err := NFOIdentity([]byte(tc.xml), tc.kind)
		if err != nil || id != tc.id {
			t.Fatal(tc, id, err)
		}
	}
	for _, s := range []string{`<tvshow><tmdbid>100</tmdbid></tvshow>`, `<movie><tmdbid>01</tmdbid></movie>`, `<movie><tmdbid>-1</tmdbid></movie>`, `<movie><tmdbid>123</tmdbid><uniqueid type="tmdb">456</uniqueid></movie>`, `<movie><tmdbid><a>123</a></tmdbid></movie>`, `<movie><uniqueid type="tmdb" type="imdb">123</uniqueid></movie>`, `<!DOCTYPE movie [<!ENTITY e SYSTEM "file:///etc/passwd">]><movie><tmdbid>&e;</tmdbid></movie>`, `<movie/><movie/>`, `<movie><tmdbid>100</tmdbid>`, `<x:movie xmlns:x="urn:movie"/>`, `<movie/>unparsed`, strings.Repeat("x", (2<<20)+1)} {
		if id, err := NFOIdentity([]byte(s), "movie"); err == nil {
			t.Fatal("unsafe NFO accepted", id, s[:min(len(s), 100)])
		}
	}
}
