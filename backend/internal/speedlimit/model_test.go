package speedlimit

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestLegacySpecAndSmartAllocation(t *testing.T) {
	s, err := DecodeSpec(`{"upload_limit":"32.9","download_limit":128,"upload_unlimit":"0","download_unlimit":12,"bandwidth":"100.9","interval":"","ipv4":"192.168.0.0/16","ipv6":"fd00::/8","downloaders":[9223372036854775807,"2"],"allocation_ratio":"1:3","notify":true,"unknown":{"n":9007199254740993}}`)
	if err != nil || s.Upload != 32 || s.Bandwidth != 100 || s.Interval != 300 || s.Downloaders[0] != "9223372036854775807" || !s.Notify {
		t.Fatal(s, err)
	}
	targets := []Target{{ID: s.Downloaders[0], Weight: 1}, {ID: "2", Weight: 3}}
	got, err := s.Limits(Observation{Playing: 1, Bitrate: 18080000}, targets)
	if err != nil || !reflect.DeepEqual(got, [][2]int64{{128, 2500}, {128, 7500}}) {
		t.Fatal(got, err)
	}
	got, err = s.Limits(Observation{}, targets)
	if err != nil || !reflect.DeepEqual(got, [][2]int64{{12, 0}, {12, 0}}) {
		t.Fatal(got, err)
	}
	got, err = s.Limits(Observation{Playing: 1, Bitrate: 101000000}, targets)
	if err != nil || got[0][1] != 10 || got[1][1] != 10 {
		t.Fatal(got, err)
	}
	for _, o := range []Observation{{Playing: 1}, {Playing: 1, UnknownBitrate: 1, Bitrate: 2}, {Playing: -1}, {Playing: 1, UnknownBitrate: 2}, {Playing: 1000, Exempt: 1}, {Bitrate: 1 << 51}} {
		if _, err := s.Limits(o, targets); err == nil {
			t.Fatal("invalid observation", o)
		}
	}
	s.Bandwidth = 0
	if _, err := s.Limits(Observation{Playing: 1, UnknownBitrate: 1}, targets); err != nil {
		t.Fatal("fixed limits require no fabricated bitrate", err)
	}
	for _, ratio := range []string{"1", "1:0", "x:2", "1:1000001"} {
		s, err := DecodeSpec(`{"downloaders":[1,2],"allocation_ratio":"` + ratio + `"}`)
		if err != nil || len(s.Ratios) != 0 {
			t.Fatal(ratio, s, err)
		}
	}
	s, err = DecodeSpec(`{"downloaders":[1,2],"allocation_ratio":" 1 : 3 "}`)
	if err != nil || !reflect.DeepEqual(s.Ratios, []int64{1, 3}) {
		t.Fatal("legacy integer whitespace lost", s.Ratios, err)
	}
}
func TestSpecRejectsMalformedScopeAndUnsafeValues(t *testing.T) {
	for _, raw := range []string{`null`, `{} {}`, `{"downloaders":[1,1]}`, `{"downloaders":[1.1]}`, `{"downloaders":["01"]}`, `{"downloaders":[9223372036854775808]}`, `{"notify":"true"}`, `{"bandwidth":1000001}`, `{"interval":0}`, `{"interval":86401}`, `{"upload_limit":"NaN"}`, `{"upload_limit":-1}`, `{"ipv4":"192.168.1.1/24"}`, `{"ipv6":"::ffff:192.168.0.0/120"}`, `{"ipv4":"::/0"}`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := DecodeSpec(raw); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	a := RawConfigDigest(`{"b":9007199254740993,"a":true}`)
	b := RawConfigDigest(`{"a":true,"b":9007199254740993}`)
	if a != b || a == RawConfigDigest(`{"b":9007199254740992,"a":true}`) {
		t.Fatal("large configuration integer lost")
	}
}
func TestExemptAddressesAndLegacyAllRanges(t *testing.T) {
	s := Spec{IPv4: "192.168.0.0/16,127.0.0.1", IPv6: "fd00::/8"}
	for _, tc := range []struct {
		address string
		want    bool
	}{{"192.168.1.8", true}, {"192.168.1.8:456", true}, {"::ffff:192.168.1.8", true}, {"[fd12::1]:9000", true}, {"8.8.8.8", false}, {"2001:db8::1", false}} {
		got, err := s.ExemptAddress(tc.address)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	for _, v := range []string{"", "invalid", "fe80::1%en0", "192.168.1.1,8.8.8.8"} {
		if _, err := s.ExemptAddress(v); err == nil {
			t.Fatal(v)
		}
	}
	s, _ = DecodeSpec("")
	for _, v := range []string{"8.8.8.8", "2001:db8::1"} {
		if got, err := s.ExemptAddress(v); err != nil || !got {
			t.Fatal(got, err)
		}
	}
}
func TestGlobalRateUnitsAndAlternateModes(t *testing.T) {
	q := Rates{Unit: "B/s", Alternative: true}
	got, err := q.Desired("qbittorrent", 32, 64)
	if err != nil || got.Upload != 65536 || got.Download != 32768 || !got.Alternative || !got.UploadLimited {
		t.Fatal(got, err)
	}
	zero, err := got.Desired("qbittorrent", 0, 0)
	if err != nil || zero.UploadLimited || zero.DownloadLimited {
		t.Fatal(zero, err)
	}
	if _, err := q.Desired("qbittorrent", 1<<22, 0); err == nil {
		t.Fatal("older qB int32 setter overflow")
	}
	tr := Rates{Unit: "RPC KB/s", Upload: 0.5, Download: 4.5, Scheduled: true, OtherUpload: 50, OtherDownload: 70}
	got, err = tr.Desired("transmission", 32, 64)
	if err != nil || got.Upload != 64 || got.Download != 32 || got.OtherUpload != 50 || !got.Scheduled {
		t.Fatal(got, err)
	}
	tr.Alternative = true
	if _, err := tr.Desired("transmission", 32, 64); err != ErrOverride {
		t.Fatal(err)
	}
	for _, n := range []float64{math.NaN(), math.Inf(1), -1} {
		tr.Upload = n
		if tr.Valid("transmission") {
			t.Fatal(n)
		}
	}
	q.Upload = 0.5
	if q.Valid("qbittorrent") {
		t.Fatal("fractional bytes")
	}
}
func TestControlValidation(t *testing.T) {
	c := Control{Epoch: strings.Repeat("a", 32), Actor: "admin", Authority: strings.Repeat("a", 64), ConfigDigest: strings.Repeat("b", 64), MediaDigest: strings.Repeat("c", 64), Server: "emby", Created: nowString(), Baselines: []Baseline{}}
	raw, _ := json.Marshal(c)
	if _, err := DecodeControl(string(raw)); err != nil {
		t.Fatal(err)
	}
	c.Authorized = true
	raw, _ = json.Marshal(c)
	if _, err := DecodeControl(string(raw)); err == nil {
		t.Fatal("empty enabled scope")
	}
}
