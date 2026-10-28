package mediaserver

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

var webhookBodies = map[string]string{
	"emby":     `{"Event":"playback.start","EventId":"event-one","Item":{"Id":"item-1","Type":"Episode","Name":"第一集","SeriesName":"剧集","ParentIndexNumber":2,"IndexNumber":3,"RunTimeTicks":100,"Overview":"剧情"},"PlaybackInfo":{"PositionTicks":25},"Session":{"Client":"Web","DeviceName":"客厅","RemoteEndPoint":"192.0.2.8"},"User":{"Name":"viewer"},"ApiKey":"raw-secret","Url":"https://evil.test/private"}`,
	"jellyfin": `{"NotificationType":"PlaybackStart","NotificationId":"event-one","ItemId":"item-1","ItemType":"Episode","Name":"第一集","SeriesName":"剧集","SeasonNumber":2,"EpisodeNumber":3,"RunTimeTicks":100,"PlaybackPositionTicks":25,"NotificationUsername":"viewer","ClientName":"Web","DeviceName":"客厅","RemoteEndPoint":"192.0.2.8","Token":"raw-secret","ServerUrl":"https://evil.test/private"}`,
	"plex":     `{"event":"media.play","Metadata":{"ratingKey":"item-1","type":"episode","title":"第一集","grandparentTitle":"剧集","parentIndex":2,"index":3,"duration":100,"viewOffset":25,"summary":"剧情"},"Account":{"title":"viewer","thumb":"https://evil.test/raw-secret"},"Player":{"title":"Web","publicAddress":"192.0.2.8"},"Server":{"uuid":"server"},"token":"raw-secret"}`,
}

func TestWebhookNormalizeAllProvidersWithoutPersistingRawSecretsOrURLs(t *testing.T) {
	for server, body := range webhookBodies {
		t.Run(server, func(t *testing.T) {
			e, identity, err := ParseWebhook(server, []byte(body))
			if err != nil || e.Action != "playback.start" || e.MediaType != "TV" || e.Name != "剧集 S2E3 第一集" || e.ItemID != "item-1" || e.Percentage == nil || *e.Percentage != 25 || e.User != "viewer" {
				t.Fatal(e, identity, err)
			}
			raw, _ := json.Marshal(e)
			if strings.Contains(string(raw), "raw-secret") || strings.Contains(string(raw), "evil.test") || strings.Contains(string(raw), "ServerUrl") {
				t.Fatal(string(raw))
			}
			title, text := e.Message("2026-10-27T12:00:00Z")
			if !strings.Contains(title, "开始播放剧集") || !strings.Contains(text, "25.00%") || !strings.Contains(text, "viewer") {
				t.Fatal(title, text)
			}
		})
	}
}
func TestWebhookSupportedActionsAndNoGuessedSeasonNumbers(t *testing.T) {
	for _, action := range []string{"library.new", "system.webhooktest", "playback.start", "playback.stop", "user.authenticated", "user.authenticationfailed", "item.rate"} {
		raw := `{"Event":"` + action + `","Item":{"Type":"Movie","Name":"电影","ProductionYear":2026}}`
		e, _, err := ParseWebhook("emby", []byte(raw))
		if err != nil || e.Action != action {
			t.Fatal(action, e, err)
		}
	}
	for action, want := range map[string]string{"PlaybackStart": "playback.start", "PlaybackStop": "playback.stop", "ItemAdded": "library.new", "AuthenticationSuccess": "user.authenticated", "AuthenticationFailure": "user.authenticationfailed"} {
		e, _, err := ParseWebhook("jellyfin", []byte(`{"NotificationType":"`+action+`","Name":"item"}`))
		if err != nil || e.Action != want {
			t.Fatal(action, e, err)
		}
	}
	e, _, err := ParseWebhook("emby", []byte(`{"Event":"playback.start","Title":"旧 GET 标题"}`))
	if err != nil || e.Name != "旧 GET 标题" {
		t.Fatal(e, err)
	}
	e, _, err = ParseWebhook("jellyfin", []byte(`{"NotificationType":"PlaybackStart","ItemType":"Episode","Name":"集","SeriesName":"剧"}`))
	if err != nil || e.Name != "剧 集" || strings.Contains(e.Name, "S0") {
		t.Fatal(e, err)
	}
	for _, raw := range []string{`{"event":"media.pause","Metadata":{"type":"movie","title":"M"}}`, `{"event":"media.play","Metadata":{"type":"movie","title":"M","live":true}}`, `{"event":"library.new","Metadata":{"type":"track","title":"M"}}`} {
		if _, _, err := ParseWebhook("plex", []byte(raw)); !errors.Is(err, ErrWebhookIgnored) {
			t.Fatal(raw, err)
		}
	}
}
func TestWebhookStrictJSONBoundsNumbersAndUntrustedFields(t *testing.T) {
	for _, raw := range []string{`{"Event":"playback.start","Event":"playback.stop","Title":"M"}`, `{"Event":"playback.start","Item":{"Name":"a","Name":"b"}}`, `[]`, `null`, `{} {}`, `{"Event":"playback.start","Item":[]}`, `{"Event":"playback.start","Title":true}`, `{"Event":"playback.start","Title":"a\u0000"}`, `{"Event":"playback.start","Title":"M","TranscodingInfo":{"CompletionPercentage":101}}`, `{"Event":"playback.start","Title":"M","PlaybackInfo":{"PositionTicks":-1}}`} {
		if _, _, err := ParseWebhook("emby", []byte(raw)); err == nil {
			t.Fatal("accepted invalid notification", raw)
		}
	}
	if _, _, err := ParseWebhook("emby", []byte(strings.Repeat(" ", WebhookBodyLimit+1))); err == nil {
		t.Fatal("oversized body")
	}
	for _, n := range []int{0, 100, 1000} {
		e, _, err := ParseWebhook("jellyfin", []byte(`{"NotificationType":"PlaybackStop","Name":"M","PlaybackPositionTicks":`+string(mustWebhookJSON(n))+`,"RunTimeTicks":100}`))
		if err != nil || e.Percentage == nil || *e.Percentage > 100 {
			t.Fatal(e, err)
		}
	}
	e, _, err := ParseWebhook("emby", []byte(`{"Event":"playback.stop","Title":"M","PlaybackInfo":{"PositionTicks":10},"Item":{"RunTimeTicks":0}}`))
	if err != nil || e.Percentage != nil {
		t.Fatal("zero runtime must not divide", e, err)
	}
	e.Link = "https://user:password@evil.test/"
	if e.Validate() == nil {
		t.Fatal("credential-bearing playback link")
	}
}
func mustWebhookJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }
func TestWebhookExplicitIdentityAndCanonicalPayloadDigest(t *testing.T) {
	epoch := strings.Repeat("a", 32)
	a, err := WebhookReceiptID(epoch, "emby", "known-event", "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := WebhookReceiptID(epoch, "emby", "known-event", "")
	c, _ := WebhookReceiptID(strings.Repeat("b", 32), "emby", "known-event", "")
	if a != b || a == c {
		t.Fatal(a, b, c)
	}
	first, _ := WebhookReceiptID(epoch, "emby", "", strings.Repeat("c", 32))
	second, _ := WebhookReceiptID(epoch, "emby", "", strings.Repeat("d", 32))
	if first == second {
		t.Fatal("missing sender identity collapsed distinct playback")
	}
	d1, _ := WebhookPayloadDigest("emby", []byte(`{"Event":"system.webhooktest","x":1}`))
	d2, _ := WebhookPayloadDigest("emby", []byte(`{ "x":1, "Event":"system.webhooktest" }`))
	if d1 != d2 {
		t.Fatal("formatting changed explicit identity payload", d1, d2)
	}
	for _, id := range []string{" leading", "line\nbreak", strings.Repeat("a", 513)} {
		if _, err := WebhookReceiptID(epoch, "emby", id, ""); err == nil {
			t.Fatal(id)
		}
	}
}
