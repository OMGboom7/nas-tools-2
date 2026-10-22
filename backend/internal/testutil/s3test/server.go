// Package s3test supplies an isolated, in-memory S3 protocol fixture. It is
// imported by tests only; it neither contacts a cloud service nor reads accounts.
package s3test

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const AccessKey = "fixture-access"
const SecretKey = "fixture-secret-not-for-production"

type Request struct{ Method, Key, CopySource, Query, Authorization, Range, IfMatch string }
type object struct {
	data     []byte
	modified time.Time
}
type upload struct {
	key   string
	parts map[int][]byte
}
type Server struct {
	HTTP       *httptest.Server
	CA         []byte
	mu         sync.Mutex
	objects    map[string]object
	uploads    map[string]*upload
	requests   []Request
	failPrefix string
	failStatus int
	failCopy   bool
	afterWrite func()
	closed     bool
	rangeFault string
}

func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{objects: map[string]object{}, uploads: map[string]*upload{}}
	s.HTTP = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	s.CA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.HTTP.Certificate().Raw})
	t.Cleanup(s.Close)
	return s
}
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.HTTP.Close()
}
func (s *Server) Put(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = object{append([]byte{}, data...), time.Now().UTC().Truncate(time.Second)}
}
func (s *Server) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	return append([]byte{}, o.data...), ok
}
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := []string{}
	for name := range s.objects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request{}, s.requests...)
}
func (s *Server) FailNextWrite(prefix string, copyOnly bool, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failPrefix, s.failCopy, s.failStatus = prefix, copyOnly, status
}

// The one-shot callback runs after a write, before its response, under the
// fixture lock. It may revoke external configuration, but must not call Server.
func (s *Server) AfterNextWrite(fn func()) { s.mu.Lock(); defer s.mu.Unlock(); s.afterWrite = fn }

// RangeFault simulates a server ignoring, misreporting, truncating or corrupting
// a range while its full read/listing/HEAD still report the original object.
func (s *Server) RangeFault(mode string) { s.mu.Lock(); defer s.mu.Unlock(); s.rangeFault = mode }
func etag(b []byte) string               { h := md5.Sum(b); return hex.EncodeToString(h[:]) }
func replyXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	replyXML(w, struct {
		XMLName       xml.Name `xml:"Error"`
		Code, Message string
	}{Code: code, Message: "fixture error; " + SecretKey})
}
func (s *Server) doneWrite(w http.ResponseWriter, r *http.Request, key string, body any) {
	if s.afterWrite != nil {
		fn := s.afterWrite
		s.afterWrite = nil
		fn()
	}
	if s.failStatus != 0 && strings.HasPrefix(key, s.failPrefix) && (!s.failCopy || r.Header.Get("X-Amz-Copy-Source") != "") {
		status := s.failStatus
		s.failStatus = 0
		failure(w, status, "InternalError")
		return
	}
	if body != nil {
		replyXML(w, body)
	}
}
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/")
	s.requests = append(s.requests, Request{r.Method, key, r.Header.Get("X-Amz-Copy-Source"), r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Range"), r.Header.Get("If-Match")})
	if !strings.Contains(r.Header.Get("Authorization"), AccessKey) {
		failure(w, 403, "AccessDenied")
		return
	}
	bucket, objectName, _ := strings.Cut(key, "/")
	if bucket != "bucket" {
		failure(w, 404, "NoSuchBucket")
		return
	}
	q := r.URL.Query()
	if r.Method == "GET" && q.Has("location") {
		replyXML(w, struct {
			XMLName xml.Name `xml:"LocationConstraint"`
			Value   string   `xml:",chardata"`
		}{Value: "us-east-1"})
		return
	}
	if r.Method == "HEAD" && objectName == "" {
		w.WriteHeader(200)
		return
	}
	if r.Method == "GET" && objectName == "" {
		s.list(w, r, bucket)
		return
	}
	if id := q.Get("uploadId"); id != "" {
		u, ok := s.uploads[id]
		if !ok || u.key != key {
			failure(w, 404, "NoSuchUpload")
			return
		}
		if r.Method == "DELETE" {
			delete(s.uploads, id)
			w.WriteHeader(204)
			return
		}
		if r.Method == "PUT" {
			n, e := strconv.Atoi(q.Get("partNumber"))
			if e != nil || n < 1 {
				failure(w, 400, "InvalidArgument")
				return
			}
			data, e := s.payload(r)
			if e != nil {
				failure(w, 400, "InvalidRequest")
				return
			}
			u.parts[n] = data
			w.Header().Set("ETag", `"`+etag(data)+`"`)
			if r.Header.Get("X-Amz-Copy-Source") != "" {
				s.doneWrite(w, r, key, struct {
					XMLName            xml.Name `xml:"CopyPartResult"`
					ETag, LastModified string
				}{ETag: `"` + etag(data) + `"`, LastModified: time.Now().UTC().Format(time.RFC3339)})
			} else {
				s.doneWrite(w, r, key, nil)
			}
			return
		}
		if r.Method == "POST" {
			var input struct {
				Parts []struct {
					Number int `xml:"PartNumber"`
					ETag   string
				} `xml:"Part"`
			}
			if xml.NewDecoder(r.Body).Decode(&input) != nil {
				failure(w, 400, "InvalidRequest")
				return
			}
			var data []byte
			for i, p := range input.Parts {
				part, ok := u.parts[p.Number]
				if !ok || p.Number != i+1 || strings.Trim(p.ETag, `"`) != etag(part) {
					failure(w, 400, "InvalidPart")
					return
				}
				data = append(data, part...)
			}
			s.objects[key] = object{data, time.Now().UTC().Truncate(time.Second)}
			delete(s.uploads, id)
			s.doneWrite(w, r, key, struct {
				XMLName                     xml.Name `xml:"CompleteMultipartUploadResult"`
				Location, Bucket, Key, ETag string
			}{Location: s.HTTP.URL + "/" + key, Bucket: bucket, Key: objectName, ETag: `"` + etag(data) + `"`})
			return
		}
	}
	if r.Method == "POST" && q.Has("uploads") {
		id := strconv.Itoa(len(s.requests))
		s.uploads[id] = &upload{key: key, parts: map[int][]byte{}}
		replyXML(w, struct {
			XMLName               xml.Name `xml:"InitiateMultipartUploadResult"`
			Bucket, Key, UploadId string
		}{Bucket: bucket, Key: objectName, UploadId: id})
		return
	}
	o, exists := s.objects[key]
	if r.Method == "HEAD" || r.Method == "GET" {
		if !exists {
			failure(w, 404, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		w.Header().Set("ETag", `"`+etag(o.data)+`"`)
		w.Header().Set("Last-Modified", o.modified.Format(http.TimeFormat))
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Method == "GET" {
			data := o.data
			if bounds := r.Header.Get("Range"); bounds != "" && s.rangeFault != "ignore" {
				var first, last int
				if _, err := fmt.Sscanf(bounds, "bytes=%d-%d", &first, &last); err != nil || first < 0 || first > last || last >= len(data) {
					w.Header().Del("Content-Length")
					failure(w, 416, "InvalidRange")
					return
				}
				data = append([]byte{}, data[first:last+1]...)
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(o.data)))
				switch s.rangeFault {
				case "bounds":
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first+1, last+1, len(o.data)))
				case "corrupt":
					data[0] ^= 1
				case "short":
					data = data[:len(data)-1]
				}
				w.WriteHeader(http.StatusPartialContent)
			}
			_, _ = w.Write(data)
		}
		return
	}
	if r.Method == "PUT" {
		if match := r.Header.Get("If-Match"); match != "" && (!exists || strings.Trim(match, `"`) != etag(o.data)) {
			failure(w, 412, "PreconditionFailed")
			return
		}
		if exists && r.Header.Get("If-None-Match") == "*" {
			failure(w, 412, "PreconditionFailed")
			return
		}
		data, err := s.payload(r)
		if err != nil {
			failure(w, 400, "InvalidRequest")
			return
		}
		s.objects[key] = object{data, time.Now().UTC().Truncate(time.Second)}
		w.Header().Set("ETag", `"`+etag(data)+`"`)
		var body any
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			body = struct {
				XMLName            xml.Name `xml:"CopyObjectResult"`
				ETag, LastModified string
			}{ETag: `"` + etag(data) + `"`, LastModified: time.Now().UTC().Format(time.RFC3339)}
		}
		s.doneWrite(w, r, key, body)
		return
	}
	failure(w, 405, "MethodNotAllowed")
}
func (s *Server) payload(r *http.Request) ([]byte, error) {
	if source := r.Header.Get("X-Amz-Copy-Source"); source != "" {
		source, err := url.PathUnescape(strings.TrimPrefix(source, "/"))
		if err != nil {
			return nil, err
		}
		o, ok := s.objects[source]
		if !ok {
			return nil, fmt.Errorf("missing source")
		}
		if want := r.Header.Get("X-Amz-Copy-Source-If-Match"); want != "" && strings.Trim(want, `"`) != etag(o.data) {
			return nil, fmt.Errorf("source changed")
		}
		data := o.data
		if bounds := r.Header.Get("X-Amz-Copy-Source-Range"); bounds != "" {
			var a, b int
			if _, err := fmt.Sscanf(bounds, "bytes=%d-%d", &a, &b); err != nil || a < 0 || b >= len(data) || a > b {
				return nil, fmt.Errorf("range")
			}
			data = data[a : b+1]
		}
		return append([]byte{}, data...), nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, (80<<20)+1))
	if err != nil || len(data) > 80<<20 {
		return nil, fmt.Errorf("body limit")
	}
	if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		reader := bytes.NewBuffer(data)
		data = nil
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return nil, err
			}
			size, _, _ := strings.Cut(strings.TrimSpace(line), ";")
			n, err := strconv.ParseInt(size, 16, 64)
			if err != nil || n < 0 || n > 80<<20 {
				return nil, fmt.Errorf("chunk")
			}
			if n == 0 {
				break
			}
			if int64(reader.Len()) < n+2 {
				return nil, fmt.Errorf("short chunk")
			}
			data = append(data, reader.Next(int(n))...)
			if !bytes.Equal(reader.Next(2), []byte("\r\n")) {
				return nil, fmt.Errorf("chunk suffix")
			}
		}
	}
	if want := r.Header.Get("Content-Md5"); want != "" {
		h := md5.Sum(data)
		if base64.StdEncoding.EncodeToString(h[:]) != want {
			return nil, fmt.Errorf("content checksum")
		}
	}
	return data, nil
}
func (s *Server) list(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	names := []string{}
	folders := map[string]bool{}
	for key := range s.objects {
		if strings.HasPrefix(key, bucket+"/"+prefix) {
			relative := strings.TrimPrefix(key, bucket+"/"+prefix)
			if delimiter := q.Get("delimiter"); delimiter != "" {
				if before, _, found := strings.Cut(relative, delimiter); found {
					folder := bucket + "/" + prefix + before + delimiter
					if !folders[folder] {
						names = append(names, folder)
						folders[folder] = true
					}
					continue
				}
			}
			names = append(names, key)
		}
	}
	sort.Strings(names)
	start, _ := strconv.Atoi(q.Get("continuation-token"))
	if start < 0 || start > len(names) {
		failure(w, 400, "InvalidArgument")
		return
	}
	max := 1000
	if n, _ := strconv.Atoi(q.Get("max-keys")); n > 0 && n < max {
		max = n
	}
	end := start + max
	if end > len(names) {
		end = len(names)
	}
	type entry struct {
		Key, LastModified, ETag string
		Size                    int
		StorageClass            string
	}
	result := struct {
		XMLName                    xml.Name `xml:"ListBucketResult"`
		Name, Prefix, EncodingType string
		KeyCount, MaxKeys          int
		IsTruncated                bool
		NextContinuationToken      string `xml:",omitempty"`
		Contents                   []entry
		CommonPrefixes             []struct{ Prefix string }
	}{Name: bucket, Prefix: prefix, EncodingType: "url", MaxKeys: max, IsTruncated: end < len(names), Contents: []entry{}}
	if result.IsTruncated {
		result.NextContinuationToken = strconv.Itoa(end)
	}
	for _, key := range names[start:end] {
		name := strings.TrimPrefix(key, bucket+"/")
		name = strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
		if folders[key] {
			result.CommonPrefixes = append(result.CommonPrefixes, struct{ Prefix string }{name})
			continue
		}
		o := s.objects[key]
		result.Contents = append(result.Contents, entry{Key: name, LastModified: o.modified.Format(time.RFC3339), ETag: `"` + etag(o.data) + `"`, Size: len(o.data), StorageClass: "STANDARD"})
	}
	result.KeyCount = len(result.Contents) + len(result.CommonPrefixes)
	replyXML(w, result)
}
