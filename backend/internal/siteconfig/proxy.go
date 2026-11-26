package siteconfig

import (
	"net/http"
	"net/url"
	"strings"
)

// ProxyTransport applies one site's configured proxy without modifying other clients.
func ProxyTransport(transport http.RoundTripper, raw string) (http.RoundTripper, error) {
	proxy, err := ProxyURL(raw)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	base, ok := transport.(*http.Transport)
	if !ok {
		return nil, http.ErrNotSupported
	}
	configured := base.Clone()
	configured.Proxy = http.ProxyURL(proxy)
	return configured, nil
}

// ProxyURL validates the endpoint independently of the transport so redirect
// handlers can select the configured proxy for each destination scheme.
func ProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	proxy, err := url.Parse(raw)
	if raw == "" || err != nil || proxy.Hostname() == "" || proxy.RawQuery != "" || proxy.Fragment != "" || (proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") {
		return nil, http.ErrNotSupported
	}
	return proxy, nil
}
