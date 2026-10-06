package mediameta

import (
	"context"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestReleaseGroupCatalogAndBoundaries(t *testing.T) {
	for _, test := range []struct{ title, want string }{
		{"Movie.1080p-WiKi", "WiKi"},
		{"Movie-WiKi@CHD@WiKi", "WiKi@CHD"},
		{"[LoliHouse] Anime 【织梦字幕组】", "LoliHouse@织梦字幕组"},
		{"Movie &FRDS&WiKi", "FRDS@WiKi"},
		{"Movie￡OurBits", "OurBits"},
		{"WiKi Documentary 2025", ""},
		{"Movie-WiKiExtra", ""},
		{"Movie-UnknownTeam", ""},
		{"Movie-NTb@FLUX@HONEyG@MTeamTV@Lilith-Raws", "NTb@FLUX@HONEyG@MTeamTV@Lilith-Raws"},
	} {
		team, custom, err := MatchLabels(t.Context(), test.title, LabelOptions{})
		if err != nil || team != test.want || custom != "" {
			t.Errorf("%q => %q/%q err=%v", test.title, team, custom, err)
		}
	}
}

func TestCustomLabelsPreserveLegacyOrderAndCaptureValues(t *testing.T) {
	options := LabelOptions{ReleaseGroups: `MyTeam;Extra(?:HD|TV)`, ReleaseSeparator: "+", Customization: `HDR;WEB;DTS(-HD)?`, CustomSeparator: "_"}
	team, custom, err := MatchLabels(t.Context(), "Movie WEB DTS-HD HDR WEB-ExtraHD@MyTeam", options)
	if err != nil || team != "ExtraHD+MyTeam" || custom != "HDR_WEB_DTS-HD_-HD" {
		t.Fatalf("labels=%q/%q err=%v", team, custom, err)
	}
	metadata, err := ParseWithOptions(t.Context(), "[MyTeam] Movie.2025.1080p.WEB-DL.HDR", "", options)
	if err != nil || metadata.Title != "Movie" || metadata.Team != "MyTeam" || metadata.Customization != "HDR_WEB" {
		t.Fatalf("metadata=%+v err=%v", metadata, err)
	}
	if _, _, err := MatchLabels(t.Context(), "Movie", LabelOptions{Customization: "[invalid"}); err == nil {
		t.Fatal("invalid regex accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := MatchLabels(ctx, "No matching groups", LabelOptions{}); err == nil {
		t.Fatal("canceled processing succeeded")
	}
	if _, _, err := MatchLabels(t.Context(), strings.Repeat("x", 65537), LabelOptions{}); err == nil {
		t.Fatal("oversized title accepted")
	}
}

func TestLabelConfigurationHonorsInstalledStateAndReloads(t *testing.T) {
	store, err := systemconfig.Open(t.TempDir() + "/user.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Set(t.Context(), "plugin.CustomReleaseGroups", `{"release_groups":"MyTeam","separator":"+","unknown":true}`); err != nil {
		t.Fatal(err)
	}
	options, err := ReadLabelOptions(t.Context(), store)
	if err != nil || options.ReleaseGroups != "" {
		t.Fatalf("uninstalled options=%+v err=%v", options, err)
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["CustomReleaseGroups","Customization","Other"]`); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(t.Context(), "plugin.Customization", `{"customization":"HDR;WEB","separator":"_"}`); err != nil {
		t.Fatal(err)
	}
	options, err = ReadLabelOptions(t.Context(), store)
	if err != nil || options.ReleaseGroups != "MyTeam" || options.ReleaseSeparator != "+" || options.Customization != "HDR;WEB" || options.CustomSeparator != "_" {
		t.Fatalf("options=%+v err=%v", options, err)
	}
	if err := store.Set(t.Context(), "plugin.CustomReleaseGroups", `{"release_groups":"UpdatedTeam"}`); err != nil {
		t.Fatal(err)
	}
	options, err = ReadLabelOptions(t.Context(), store)
	if err != nil || options.ReleaseGroups != "UpdatedTeam" {
		t.Fatalf("updated options=%+v err=%v", options, err)
	}
}
