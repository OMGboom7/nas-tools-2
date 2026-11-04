package cookiecloud

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

type Candidate struct {
	Original siteconfig.Site
	Cookie   string
}

func hostName(raw string) string {
	raw = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(raw), "."), "."))
	if net.ParseIP(raw) != nil {
		return raw
	}
	normalized, e := idna.Lookup.ToASCII(raw)
	if e != nil || normalized == "" || strings.ContainsAny(normalized, "/:@%[]*") {
		return ""
	}
	return normalized
}

func siteAddress(raw string) *url.URL {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || hostName(u.Hostname()) == "" {
		return nil
	}
	return u
}

func group(host string) string {
	if host == "" {
		return ""
	}
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return host
	}
	key, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return key
}

func filterGroups(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '，' || unicode.IsSpace(r) }) {
		if !strings.Contains(part, "://") {
			part = "https://" + part
		}
		if u := siteAddress(part); u != nil {
			if g := group(hostName(u.Hostname())); g != "" {
				out[g] = true
			}
		}
	}
	return out
}

type scopedCookie struct {
	Cookie
	host     string
	hostOnly bool
}

func cookieHeader(u *url.URL, cookies []scopedCookie) string {
	host := hostName(u.Hostname())
	values := map[string]string{}
	fields := []string{}
	usable := false
	for _, c := range cookies {
		if (c.Secure && u.Scheme != "https") || (c.host != host && (c.hostOnly || !strings.HasSuffix(host, "."+c.host))) {
			continue
		}
		if previous, exists := values[c.Name]; exists {
			if previous != c.Value {
				return ""
			}
			continue
		}
		values[c.Name] = c.Value
		fields = append(fields, c.Name+"="+c.Value)
		usable = usable || (c.Name != "cf_clearance" && c.Value != "")
	}
	header := strings.Join(fields, "; ")
	if !usable || len(header) > 16000 {
		return ""
	}
	return header
}

// The site table has one Cookie header for both login and RSS requests.
// Only publish to sites whose configured receivers share the same origin.
func sharedSiteAddress(site siteconfig.Site) *url.URL {
	sign, rss := siteAddress(site.SignURL), siteAddress(site.RSSURL)
	if strings.TrimSpace(site.SignURL) != "" && sign == nil || strings.TrimSpace(site.RSSURL) != "" && rss == nil {
		return nil
	}
	if sign == nil {
		return rss
	}
	if rss != nil && !sameOrigin(sign, rss) {
		return nil
	}
	return sign
}

func sameOrigin(left, right *url.URL) bool {
	return left.Scheme == right.Scheme && strings.EqualFold(left.Host, right.Host)
}

func catalogSharesOrigin(u *url.URL, receivers map[string][]*url.URL) bool {
	for _, receiver := range receivers[indexercatalog.Domain(u.String())] {
		if receiver != nil && !sameOrigin(u, receiver) {
			return false
		}
	}
	return true
}

func usesDerivedAPI(u *url.URL) bool {
	// The existing connection check can send the shared header to an API subdomain.
	return strings.Contains(strings.ToLower(u.Hostname()), "m-team")
}

func Candidates(sites []siteconfig.Site, catalog indexercatalog.Catalog, cookies []Cookie, s Settings, now time.Time) []Candidate {
	black, white := filterGroups(s.BlackList), filterGroups(s.WhiteList)
	byGroup := map[string][]scopedCookie{}
	for _, c := range cookies {
		host := hostName(c.Domain)
		key := group(host)
		if key == "" || (s.Mode == "black_mode" && black[key]) || (s.Mode == "white_mode" && !white[key]) ||
			(c.Path != "" && c.Path != "/") || (c.Expiration != 0 && c.Expiration <= float64(now.Unix())) ||
			c.Name == "CookieAutoDeleteBrowsingDataCleanup" || (len(c.PartitionKey) > 0 && string(c.PartitionKey) != "null") ||
			strings.ContainsAny(c.Value, ";,\"\\\r\n") || (&http.Cookie{Name: c.Name, Value: c.Value}).Valid() != nil {
			continue
		}
		hostOnly := !strings.HasPrefix(c.Domain, ".")
		if c.HostOnly != nil {
			hostOnly = *c.HostOnly
		}
		if net.ParseIP(host) != nil || !strings.Contains(host, ".") {
			hostOnly = true
		}
		byGroup[key] = append(byGroup[key], scopedCookie{c, host, hostOnly})
	}
	out := []Candidate{}
	configured := map[string]bool{}
	names := map[string]bool{}
	hostUses := map[string]int{}
	receivers := map[string][]*url.URL{}
	for _, entry := range catalog.Indexers {
		key := indexercatalog.Domain(entry.Domain)
		receivers[key] = append(receivers[key], siteAddress(entry.Domain))
	}
	for _, site := range sites {
		names[site.Name] = true
		hosts := map[string]bool{}
		for _, raw := range []string{site.SignURL, site.RSSURL} {
			if u := siteAddress(raw); u != nil {
				hosts[hostName(u.Hostname())] = true
			}
		}
		for host := range hosts {
			hostUses[host]++
			configured[group(host)] = true
		}
	}
	for _, site := range sites {
		if u := sharedSiteAddress(site); u != nil && !usesDerivedAPI(u) && catalogSharesOrigin(u, receivers) {
			host := hostName(u.Hostname())
			if header := cookieHeader(u, byGroup[group(host)]); header != "" && hostUses[host] == 1 {
				out = append(out, Candidate{Original: site, Cookie: header})
			}
		}
	}
	// A new site must have one unambiguous catalog receiver for its group.
	potential := map[string][]Candidate{}
	for _, entry := range catalog.Indexers {
		u := siteAddress(entry.Domain)
		if u == nil || usesDerivedAPI(u) || !catalogSharesOrigin(u, receivers) || u.RawQuery != "" || u.Fragment != "" || names[entry.Name] {
			continue
		}
		key := group(hostName(u.Hostname()))
		if configured[key] {
			continue
		}
		if header := cookieHeader(u, byGroup[key]); header != "" {
			potential[key] = append(potential[key], Candidate{
				Original: siteconfig.Site{Name: entry.Name, SignURL: entry.Domain, Include: "T"}, Cookie: header,
			})
		}
	}
	for _, entry := range catalog.Indexers {
		if u := siteAddress(entry.Domain); u != nil {
			key := group(hostName(u.Hostname()))
			if p := potential[key]; len(p) == 1 && !names[p[0].Original.Name] {
				out = append(out, p[0])
				names[p[0].Original.Name] = true
				delete(potential, key)
			}
		}
	}
	return out
}
