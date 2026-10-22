package remotestore

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
	"golang.org/x/net/http/httpproxy"
)

const minioProtocol = "native:minio-go/v7.0.98/1"

type minioAlias struct {
	URL, AccessKey, SecretKey, SessionToken, API, Path string
}
type minioSettings struct {
	alias minioAlias
	ca    []byte
	proxy *httpproxy.Config
}
type minioRemote struct {
	client    *minio.Client
	transport *http.Transport
}

// Configuration is explicitly selected, never searched in a workstation home.
// Only a stable bounded snapshot is used, and neither SDK errors nor credentials
// are returned to HTTP clients or emitted by a command-line child process.
func minioSnapshot(name string) (string, []byte, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) == "/" {
		return "", nil, ErrUnavailable
	}
	canonical, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", nil, ErrUnavailable
	}
	f, err := os.Open(canonical)
	if err != nil {
		return "", nil, ErrUnavailable
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > 4<<20 {
		return "", nil, ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	after, e1 := f.Stat()
	named, e2 := os.Stat(canonical)
	if err != nil || e1 != nil || e2 != nil || len(data) > 4<<20 || !os.SameFile(before, after) || !os.SameFile(after, named) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", nil, ErrChanged
	}
	return canonical, data, nil
}

// JSON's last-key-wins behavior must not reinterpret a reviewed account.
func uniqueMinioJSON(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		t, err := d.Token()
		if err != nil {
			return false
		}
		if t == json.Delim('{') {
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				name, ok := k.(string)
				if err != nil || !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		}
		if t == json.Delim('[') {
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		}
		_, delim := t.(json.Delim)
		return !delim
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func (f Factory) minioBase() (Binding, []byte, minioSettings, error) {
	var settings minioSettings
	file, data, err := minioSnapshot(f.ConfigFile)
	if err != nil || !uniqueMinioJSON(data) {
		return Binding{}, nil, settings, ErrUnavailable
	}
	var cfg struct {
		Version string
		Aliases map[string]minioAlias
		Hosts   map[string]minioAlias
	}
	if json.Unmarshal(data, &cfg) != nil {
		return Binding{}, nil, settings, ErrUnavailable
	}
	aliases := cfg.Aliases
	if cfg.Version == "9" {
		aliases = cfg.Hosts
	} else if cfg.Version != "10" {
		return Binding{}, nil, settings, ErrUnavailable
	}
	a, exists := aliases["NASTOOL"]
	env := f.Environment
	if env == nil {
		env = os.Environ()
	}
	selected := map[string]string{}
	for _, item := range env {
		name, v, ok := strings.Cut(item, "=")
		if ok && (name == "MC_HOST_NASTOOL" || strings.EqualFold(name, "http_proxy") || strings.EqualFold(name, "https_proxy") || strings.EqualFold(name, "no_proxy")) {
			if _, duplicate := selected[name]; duplicate {
				return Binding{}, nil, settings, ErrUnavailable
			}
			selected[name] = v
		}
	}
	if host := selected["MC_HOST_NASTOOL"]; host != "" {
		u, err := url.Parse(host)
		if err != nil {
			return Binding{}, nil, settings, ErrUnavailable
		}
		a = minioAlias{API: "S3v4", Path: "auto"}
		if u.User != nil {
			password, _ := u.User.Password()
			a.AccessKey = u.User.Username()
			a.SecretKey, a.SessionToken, _ = strings.Cut(password, ":")
		}
		u.User = nil
		a.URL, exists = u.String(), true
	}
	u, err := url.Parse(a.URL)
	if err != nil || !exists || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(a.URL, "\x00\r\n") || (a.AccessKey == "") != (a.SecretKey == "") || a.SessionToken != "" && a.AccessKey == "" {
		return Binding{}, nil, settings, ErrUnavailable
	}
	if a.API != "" && a.API != "S3v4" && a.API != "S3v2" {
		return Binding{}, nil, settings, ErrUnavailable
	}
	if a.Path != "" && a.Path != "auto" && a.Path != "on" && a.Path != "off" && a.Path != "dns" {
		return Binding{}, nil, settings, ErrUnavailable
	}
	caName := ""
	if f.CAFile != "" {
		caName, settings.ca, err = minioSnapshot(f.CAFile)
		if err != nil {
			return Binding{}, nil, settings, err
		}
	}
	get := func(upper, lower string) string {
		if v := selected[upper]; v != "" {
			return v
		}
		return selected[lower]
	}
	settings.proxy = &httpproxy.Config{HTTPProxy: get("HTTP_PROXY", "http_proxy"), HTTPSProxy: get("HTTPS_PROXY", "https_proxy"), NoProxy: get("NO_PROXY", "no_proxy")}
	settings.alias = a
	b := Binding{Driver: "minio", ConfigFile: file, ConfigDigest: digest(data), Binary: "native", BinaryVersion: minioProtocol, Environment: digest([]any{selected, caName, settings.ca}), Directories: true, Protocol: 1}
	return b, data, settings, nil
}

func (f Factory) openMinio(expected *Binding) (*Client, error) {
	b, _, s, err := f.minioBase()
	if err != nil {
		return nil, err
	}
	if expected != nil && b != *expected {
		return nil, ErrChanged
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, ErrUnavailable
	}
	transport := base.Clone()
	transport.Proxy = func(r *http.Request) (*url.URL, error) { return s.proxy.ProxyFunc()(r.URL) }
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.MaxResponseHeaderBytes = 1 << 20
	transport.MaxConnsPerHost = 2
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if len(s.ca) > 0 {
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(s.ca) {
			return nil, ErrUnavailable
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	u, _ := url.Parse(s.alias.URL)
	lookup := minio.BucketLookupAuto
	if s.alias.Path == "on" {
		lookup = minio.BucketLookupPath
	} else if s.alias.Path == "off" || s.alias.Path == "dns" {
		lookup = minio.BucketLookupDNS
	}
	creds := credentials.NewStaticV4(s.alias.AccessKey, s.alias.SecretKey, s.alias.SessionToken)
	if s.alias.API == "S3v2" {
		creds = credentials.NewStaticV2(s.alias.AccessKey, s.alias.SecretKey, s.alias.SessionToken)
	}
	c := &Client{factory: f, binding: b, s3: &minioRemote{transport: transport}}
	sdk, err := minio.New(u.Host, &minio.Options{Creds: creds, Secure: u.Scheme == "https", BucketLookup: lookup, Transport: minioSealedTransport{c, transport}, MaxRetries: 1})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, ErrUnavailable
	}
	c.s3.client = sdk
	return c, nil
}

type minioSealedTransport struct {
	c *Client
	t http.RoundTripper
}
type minioResponseBody struct {
	io.Reader
	io.Closer
}
type minioInventoryKey struct{}
type minioInventoryBudget struct{ bytes, pages atomic.Int64 }
type minioInventoryBody struct {
	io.ReadCloser
	budget *minioInventoryBudget
}

func (r minioInventoryBody) Read(p []byte) (int, error) {
	remaining := r.budget.bytes.Load()
	if remaining < 0 {
		return 0, ErrLimit
	}
	if int64(len(p)) > remaining+1 {
		p = p[:remaining+1]
	}
	n, err := r.ReadCloser.Read(p)
	if r.budget.bytes.Add(-int64(n)) < 0 {
		return n, ErrLimit
	}
	return n, err
}

func (t minioSealedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	budget, _ := r.Context().Value(minioInventoryKey{}).(*minioInventoryBudget)
	if budget != nil && budget.pages.Add(-1) < 0 {
		return nil, ErrLimit
	}
	if err := t.c.Current(); err != nil {
		return nil, err
	}
	// The SDK may abort its own known multipart upload, never delete an object,
	// bucket, target, retained copy, or an upload discovered from a listing.
	if r.Method == http.MethodDelete && r.URL.Query().Get("uploadId") == "" {
		return nil, ErrChanged
	}
	response, err := t.t.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if err := t.c.Current(); err != nil {
		response.Body.Close()
		return nil, err
	}
	// A HEAD response's Content-Length describes the object, not a response
	// body. Large media metadata must not be subjected to the XML body limit.
	if r.Method != http.MethodHead && (r.Method != http.MethodGet || r.URL.RawQuery != "" || response.StatusCode != 200) {
		if response.ContentLength > 16<<20 {
			response.Body.Close()
			return nil, ErrLimit
		}
		response.Body = minioResponseBody{io.LimitReader(response.Body, (16<<20)+1), response.Body}
	}
	if budget != nil {
		response.Body = minioInventoryBody{response.Body, budget}
	}
	return response, nil
}

func minioLocation(location string) (string, string, error) {
	if !safeRoot(location) {
		return "", "", ErrUnavailable
	}
	bucket, object, _ := strings.Cut(strings.TrimPrefix(location, "/"), "/")
	if s3utils.CheckValidBucketName(bucket) != nil || object != "" && !relative(object) {
		return "", "", ErrUnavailable
	}
	return bucket, object, nil
}
func minioError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrChanged) {
		return ErrChanged
	}
	if errors.Is(err, ErrLimit) {
		return ErrLimit
	}
	return ErrUnavailable
}
func minioAbsent(err error) bool {
	e := minio.ToErrorResponse(err)
	return e.Code == "NoSuchKey" || e.Code == "NoSuchObject" || e.Code == "NotFound" && e.StatusCode == 404
}

func (c *Client) minioList(ctx context.Context, location string, recursive bool) ([]Node, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	budget := &minioInventoryBudget{}
	budget.bytes.Store(16 << 20)
	budget.pages.Store(20002)
	ctx = context.WithValue(ctx, minioInventoryKey{}, budget)
	bucket, object, err := minioLocation(location)
	if err != nil {
		return nil, err
	}
	if object != "" {
		_, err := c.s3.client.StatObject(ctx, bucket, object, minio.StatObjectOptions{})
		if err == nil {
			return nil, ErrChanged // A file cannot be reinterpreted as a parent.
		}
		if !minioAbsent(err) {
			return nil, minioError(ctx, err)
		}
	}
	prefix := object
	if prefix != "" {
		prefix += "/"
	}
	nodes, rawNames := map[string]Node{}, map[string]bool{}
	bytesSeen, count := 0, 0
	add := func(n Node) error {
		if !relative(n.Path) || strings.Contains(n.Path, ".rclonelink") || strings.Count(n.Path, "/") > 64 {
			return ErrChanged
		}
		k := key(n.Path)
		if old, found := nodes[k]; found && (old.Path != n.Path || !old.IsDir || !n.IsDir) {
			return ErrChanged
		}
		nodes[k] = n
		if len(nodes) > 20000 {
			return ErrLimit
		}
		return nil
	}
	for info := range c.s3.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: recursive, MaxKeys: 1000}) {
		if info.Err != nil {
			return nil, minioError(ctx, info.Err)
		}
		count++
		bytesSeen += len(info.Key)
		if count > 20001 || bytesSeen > 16<<20 {
			return nil, ErrLimit
		}
		if !strings.HasPrefix(info.Key, prefix) || !utf8.ValidString(info.Key) || rawNames[info.Key] || info.Size < 0 {
			return nil, ErrChanged
		}
		rawNames[info.Key] = true
		if info.Key == prefix {
			// S3 returns a directory's own marker as an object when listing
			// that exact prefix. It is not a child with an empty filename.
			if !strings.HasSuffix(prefix, "/") || info.Size != 0 {
				return nil, ErrChanged
			}
			continue
		}
		name := strings.TrimPrefix(info.Key, prefix)
		directory := strings.HasSuffix(name, "/")
		if directory {
			if info.Size != 0 {
				return nil, ErrChanged
			}
			name = strings.TrimSuffix(name, "/")
		}
		if !recursive && path.Base(name) != name {
			return nil, ErrChanged
		}
		n := Node{Path: name, Name: path.Base(name), ID: info.ETag, Size: info.Size, ModTime: info.LastModified.UTC().Format(time.RFC3339Nano), IsDir: directory}
		if directory {
			n.ID, n.ModTime = "", ""
		}
		if err := add(n); err != nil {
			return nil, err
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if err := add(Node{Path: parent, Name: path.Base(parent), IsDir: true}); err != nil {
				return nil, err
			}
		}
	}
	result := []Node{}
	for _, n := range nodes {
		if recursive || path.Base(n.Path) == n.Path {
			result = append(result, n)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, c.Current()
}

func (c *Client) minioRead(ctx context.Context, location string, size int64, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	bucket, object, err := minioLocation(location)
	if err != nil || object == "" || size < 0 || size > 1<<50 {
		return ErrUnavailable
	}
	r, info, _, err := (minio.Core{Client: c.s3.client}).GetObject(ctx, bucket, object, minio.GetObjectOptions{})
	if err != nil {
		return minioError(ctx, err)
	}
	defer r.Close()
	if info.Size != size {
		return ErrChanged
	}
	n, err := io.Copy(out, io.LimitReader(r, size+1))
	if err != nil {
		return minioError(ctx, err)
	}
	if n != size {
		return ErrChanged
	}
	return c.Current()
}

func (c *Client) minioReadRange(ctx context.Context, location string, size, offset, count int64, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bucket, object, err := minioLocation(location)
	if err != nil || object == "" {
		return ErrUnavailable
	}
	options := minio.GetObjectOptions{}
	if err := options.SetRange(offset, offset+count-1); err != nil {
		return ErrLimit
	}
	r, info, headers, err := (minio.Core{Client: c.s3.client}).GetObject(ctx, bucket, object, options)
	if err != nil {
		return minioError(ctx, err)
	}
	defer r.Close()
	want := "bytes " + strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(offset+count-1, 10) + "/" + strconv.FormatInt(size, 10)
	if info.Size != count || headers.Get("Content-Range") != want {
		return ErrChanged
	}
	n, err := io.Copy(out, io.LimitReader(r, count+1))
	if err != nil {
		return minioError(ctx, err)
	}
	if n != count {
		return ErrChanged
	}
	return c.Current()
}
func (c *Client) minioUpload(ctx context.Context, location string, size int64, input io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	bucket, object, err := minioLocation(location)
	if err != nil || object == "" || size < 0 || size > 1<<50 {
		return ErrUnavailable
	}
	if n, err := c.Entry(ctx, path.Dir(location), path.Base(location)); err != nil || n != nil {
		if err != nil {
			return err
		}
		return ErrChanged
	}
	opts := minio.PutObjectOptions{NumThreads: 1, SendContentMd5: true}
	opts.SetMatchETagExcept("*")
	_, err = c.s3.client.PutObject(ctx, bucket, object, input, size, opts)
	if err != nil {
		return minioError(ctx, err)
	}
	return c.Current()
}
func (c *Client) minioCopy(ctx context.Context, from, to string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	sourceBucket, source, e1 := minioLocation(from)
	bucket, object, e2 := minioLocation(to)
	if e1 != nil || e2 != nil || object == "" || source == "" {
		return ErrUnavailable
	}
	info, err := c.s3.client.StatObject(ctx, sourceBucket, source, minio.StatObjectOptions{})
	if err != nil || info.ETag == "" {
		if err != nil {
			return minioError(ctx, err)
		}
		return ErrChanged
	}
	dst := minio.CopyDestOptions{Bucket: bucket, Object: object}
	src := minio.CopySrcOptions{Bucket: sourceBucket, Object: source, MatchETag: info.ETag}
	if info.Size > 5<<30 {
		_, err = c.s3.client.ComposeObject(ctx, dst, src)
	} else {
		_, err = c.s3.client.CopyObject(ctx, dst, src)
	}
	if err != nil {
		return minioError(ctx, err)
	}
	return c.Current()
}
func (c *Client) minioMkdir(ctx context.Context, location string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	bucket, object, err := minioLocation(location)
	if err != nil {
		return err
	}
	if object == "" {
		return c.Current() // Never create or change a bucket.
	}
	n, err := c.Entry(ctx, path.Dir(location), path.Base(location))
	if err != nil {
		return err
	}
	if n != nil {
		if !n.IsDir {
			return ErrChanged
		}
		return c.Current()
	}
	opts := minio.PutObjectOptions{DisableMultipart: true, SendContentMd5: true}
	opts.SetMatchETagExcept("*")
	_, err = c.s3.client.PutObject(ctx, bucket, object+"/", bytes.NewReader(nil), 0, opts)
	if err != nil {
		return minioError(ctx, err)
	}
	return c.Current()
}
