package cloudflare

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestOriginalSettingsUnknownValuesAndArgumentQuoting(t *testing.T) {
	raw := `{"cf_ip":"192.0.2.1","additional_args":"-dd -url 'https://fixture.invalid/a b' -n 8","unknown":{"integer":900719925474099312345,"nullable":null}}`
	s, err := DecodeSettings(raw)
	if err != nil || !s.IPv4 || s.IPv6 {
		t.Fatalf("settings=%+v err=%v", s, err)
	}
	args, err := Arguments(s.AdditionalArgs)
	if err != nil || !reflect.DeepEqual(args, []string{"-dd", "-url", "https://fixture.invalid/a b", "-n", "8"}) {
		t.Fatalf("args=%v err=%v", args, err)
	}
	patched, err := Patch(raw, map[string]json.RawMessage{"version": json.RawMessage(`"v2.3.5"`)})
	if err != nil || !strings.Contains(patched, `900719925474099312345`) || !strings.Contains(patched, `"nullable":null`) {
		t.Fatalf("patch=%s err=%v", patched, err)
	}
	for _, bad := range []string{`-f /tmp/other`, `-o=somewhere`, `--o output`, `-v`, `-url 'unterminated`, `-dd\`, strings.Repeat("x", 4097)} {
		if _, err := Arguments(bad); err == nil {
			t.Errorf("unsafe/unbound args accepted: %q", bad)
		}
	}
	args, err = Arguments(`-url '$(touch /tmp/should-never-exist)'`)
	if err != nil || args[1] != "$(touch /tmp/should-never-exist)" {
		t.Fatalf("literal substitutions changed: %v %v", args, err)
	}
}

func TestHostsPreservesOriginalShapeCommentsAndUnrelatedValues(t *testing.T) {
	for _, list := range []bool{false, true} {
		t.Run(map[bool]string{true: "list", false: "string"}[list], func(t *testing.T) {
			lines := []string{"# 192.0.2.1 comment", "  192.0.2.1\talpha.invalid # 192.0.2.1 in comment", "192.0.2.10 beta.invalid", ""}
			var value any = strings.Join(lines, "\n")
			if list {
				value = lines
			}
			encoded, _ := json.Marshal(value)
			raw := `{"enable":true,"hosts":` + string(encoded) + `,"unknown":900719925474099312345,"nil":null}`
			plan, err := PlanHosts(raw, Settings{CFIP: "192.0.2.1"}, "198.51.100.2")
			if err != nil || plan.Replaced != 1 || plan.BeforeIP != "192.0.2.1" || !strings.Contains(plan.Input, "198.51.100.2\talpha.invalid # 192.0.2.1 in comment") || !strings.Contains(plan.Input, "192.0.2.10 beta.invalid") {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			values, err := object(plan.Raw)
			if err != nil || string(values["unknown"]) != "900719925474099312345" || string(values["nil"]) != "null" {
				t.Fatalf("unknown values changed: %s", plan.Raw)
			}
			_, textFormat, err := hostLines(values["hosts"])
			if err != nil || textFormat == list {
				t.Fatalf("hosts original representation changed")
			}
			same, err := PlanHosts(raw, Settings{CFIP: "192.0.2.1"}, "192.0.2.1")
			if err != nil || same.Raw != raw {
				t.Fatalf("unchanged hosts rewritten")
			}
		})
	}
}

func TestCalibrationRequiresUniqueMostFrequentAddress(t *testing.T) {
	raw := `{"enable":true,"hosts":["# comment","192.0.2.1 a.invalid","192.0.2.1 b.invalid","192.0.2.2 c.invalid"]}`
	plan, err := PlanHosts(raw, Settings{CFIP: "192.0.2.99", Check: true}, "198.51.100.1")
	if err != nil || plan.BeforeIP != "192.0.2.1" || plan.Replaced != 2 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	tie := `{"enable":true,"hosts":["192.0.2.1 a.invalid","192.0.2.2 b.invalid"]}`
	plan, err = PlanHosts(tie, Settings{CFIP: "192.0.2.2", Check: true}, "198.51.100.1")
	if err != nil || plan.BeforeIP != "192.0.2.2" || plan.Replaced != 1 {
		t.Fatalf("tie changed original target: %+v %v", plan, err)
	}
	if _, err = PlanHosts(`{"enable":false,"hosts":"192.0.2.1 a.invalid"}`, Settings{CFIP: "192.0.2.1"}, "198.51.100.1"); err == nil {
		t.Fatal("disabled hosts accepted")
	}
}

func TestCSVOnlyAcceptsCurrentSelectedTargets(t *testing.T) {
	ranges := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/32")}
	best, n, err := csvResult([]byte("\ufeffIP 地址,延迟\n2001:db8::1,2\n192.0.2.5,3\n"), ranges)
	if err != nil || best != "2001:db8::1" || n != 2 {
		t.Fatalf("result=%s %d %v", best, n, err)
	}
	for _, bad := range []string{"IP 地址,延迟\n198.51.100.2,1\n", "IP 地址,延迟\n", "not an ip,latency\n192.0.2.1,1\n", "IP 地址,延迟\n192.0.2.1,1\nbad,2\n"} {
		if _, _, err := csvResult([]byte(bad), ranges); err == nil {
			t.Errorf("invalid result accepted: %q", bad)
		}
	}
}

func TestBothAddressFamiliesAreCombinedAndExplicitTargetsAreBound(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ip.txt"), []byte("192.0.2.0/24\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ipv6.txt"), []byte("2001:db8::/32\n"), 0600); err != nil {
		t.Fatal(err)
	}
	input, ranges, err := runInput(Installation{Root: root, Directory: "."}, Settings{IPv4: true, IPv6: true}, nil)
	if err != nil || len(ranges) != 2 || !strings.Contains(string(input), "192.0.2.0/24") || !strings.Contains(string(input), "2001:db8::/32") {
		t.Fatalf("input=%s ranges=%v err=%v", input, ranges, err)
	}
	_, ranges, err = runInput(Installation{Root: root, Directory: "."}, Settings{IPv4: true, IPv6: true}, []string{"-ip", "198.51.100.4,2001:db8:1::1"})
	if err != nil || len(ranges) != 2 || ranges[0].Addr().String() != "198.51.100.4" {
		t.Fatalf("advanced targets=%v err=%v", ranges, err)
	}
}

func TestReleaseAcceptsBothOfficialAssetNamesAndRejectsOtherOrigins(t *testing.T) {
	for _, name := range []string{"CloudflareST_linux_amd64.tar.gz", "cfst_linux_amd64.tar.gz"} {
		r := release{Version: "v2.3.5", Assets: []releaseAsset{{Name: name, URL: "https://github.com/XIU2/CloudflareSpeedTest/releases/download/v2.3.5/" + name}}}
		if _, err := selectAsset(r, "linux", "amd64"); err != nil {
			t.Fatalf("official asset rejected %s: %v", name, err)
		}
		r.Assets[0].URL = "https://fixture.invalid/" + name
		if _, err := selectAsset(r, "linux", "amd64"); err == nil {
			t.Fatal("unrelated executable origin accepted")
		}
	}
}

func TestArchiveNativeBinaryAndPathBoundaries(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{"", "../outside", "folder/../../outside", "/outside"} {
		var contents bytes.Buffer
		archive := zip.NewWriter(&contents)
		for name, data := range map[string][]byte{"cfst": binary, "ip.txt": []byte("192.0.2.0/24\n")} {
			f, err := archive.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		if extra != "" {
			f, err := archive.Create(extra)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write([]byte("must never leave archive")); err != nil {
				t.Fatal(err)
			}
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		files, err := unpack("fixture.zip", contents.Bytes(), runtime.GOOS, runtime.GOARCH)
		if extra == "" {
			if err != nil || !bytes.Equal(files["cfst"], binary) {
				t.Fatalf("native archive: %v", err)
			}
		} else if err == nil {
			t.Fatalf("path traversal accepted: %s", extra)
		}
	}
	if nativeExecutable([]byte("#!/usr/bin/env python\n"), runtime.GOOS, runtime.GOARCH) == nil {
		t.Fatal("script accepted as native executable")
	}
}

type countTransport struct{ calls int }

func (t *countTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, ErrResult
}

func TestMissingDurableIntentCannotDownloadOrCreateInstallation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new-install")
	transport := &countTransport{}
	_, err := (Installer{Root: root, Client: &http.Client{Transport: transport}}).Ensure(context.Background(), Settings{IPv4: true}, nil)
	if err == nil || transport.calls != 0 {
		t.Fatalf("unauthorized download calls=%d err=%v", transport.calls, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("installation directory created without intent: %v", err)
	}
}
