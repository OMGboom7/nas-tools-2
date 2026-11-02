package httpserver

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1" // The original WeCom callback signature uses SHA-1.
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

func signinWechatDecrypt(token, key, corp, signature, stamp, nonce, encrypted string) ([]byte, error) {
	if len(key) != 43 || len(signature) != 40 || len(nonce) > 512 || len(encrypted) > 512<<10 {
		return nil, sitesignin.ErrScope
	}
	parts := []string{token, stamp, nonce, encrypted}
	sort.Strings(parts)
	digest := sha1.Sum([]byte(strings.Join(parts, "")))
	if !constant(strings.ToLower(signature), hex.EncodeToString(digest[:])) {
		return nil, sitesignin.ErrScope
	}
	decoded, e := base64.RawStdEncoding.DecodeString(key)
	if e != nil || len(decoded) != 32 {
		return nil, sitesignin.ErrScope
	}
	block, e := aes.NewCipher(decoded)
	if e != nil {
		return nil, e
	}
	raw, e := base64.StdEncoding.DecodeString(encrypted)
	if e != nil || len(raw) < 32 || len(raw)%aes.BlockSize != 0 {
		return nil, sitesignin.ErrScope
	}
	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, decoded[:aes.BlockSize]).CryptBlocks(plain, raw)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > 32 || pad > len(plain)-20 {
		return nil, sitesignin.ErrScope
	}
	for _, v := range plain[len(plain)-pad:] {
		if int(v) != pad {
			return nil, sitesignin.ErrScope
		}
	}
	plain = plain[:len(plain)-pad]
	length := uint64(binary.BigEndian.Uint32(plain[16:20]))
	if length > uint64(len(plain)-20) {
		return nil, sitesignin.ErrScope
	}
	end := 20 + int(length)
	if !constant(string(plain[end:]), corp) {
		return nil, sitesignin.ErrScope
	}
	return plain[20:end], nil
}
func (api *siteSigninAPI) wechatHTTP(w http.ResponseWriter, r *http.Request, c sitesignin.Control, s siteSigninRemoteSource) {
	if r.Method != "GET" && r.Method != "POST" {
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, 405, 405, "不支持此企业微信操作")
		return
	}
	q := r.URL.Query()
	encrypted := q.Get("echostr")
	if r.Method == "POST" {
		raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<10))
		if e != nil {
			writeAPIError(w, 400, 400, "消息过大")
			return
		}
		var envelope struct {
			Encrypt string `xml:"Encrypt"`
		}
		if xml.Unmarshal(raw, &envelope) != nil {
			writeAPIError(w, 400, 400, "加密消息格式无效")
			return
		}
		encrypted = envelope.Encrypt
	}
	stamp, e := strconv.ParseInt(q.Get("timestamp"), 10, 64)
	if e != nil || time.Unix(stamp, 0).After(time.Now().Add(2*time.Minute)) || time.Since(time.Unix(stamp, 0)) > 5*time.Minute {
		writeAPIError(w, 403, 403, "回调时间无效")
		return
	}
	plain, e := signinWechatDecrypt(text(s.config["token"]), text(s.config["encodingAESKey"]), text(s.config["corpid"]), q.Get("msg_signature"), q.Get("timestamp"), q.Get("nonce"), encrypted)
	if e != nil {
		writeAPIError(w, 403, 403, "企业微信签名或企业身份无效")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.Method == "GET" {
		if api.base(c.Scope, "", c.Scope.PluginDigest)(r.Context()) != nil {
			signinError(w, sitesignin.ErrScope)
			return
		}
		_, _ = w.Write(plain)
		return
	}
	var m struct {
		To      string `xml:"ToUserName"`
		From    string `xml:"FromUserName"`
		Created int64  `xml:"CreateTime"`
		Type    string `xml:"MsgType"`
		Content string `xml:"Content"`
		ID      string `xml:"MsgId"`
		Agent   string `xml:"AgentID"`
		Event   string `xml:"Event"`
		Key     string `xml:"EventKey"`
	}
	if xml.Unmarshal(plain, &m) != nil {
		writeAPIError(w, 400, 400, "企业微信消息格式无效")
		return
	}
	content := m.Content
	id := m.ID
	if m.Type == "event" && m.Event == "click" {
		parts := strings.Split(m.Key, "#")
		if len(parts) > 2 && parts[2] == "_2_0" {
			content = "/pts"
		} else {
			content = m.Key
		}
		id = sitelogin.Hash([]any{m.From, m.Created, m.Agent, m.Event, m.Key})
	} else if m.Type != "text" {
		_, _ = w.Write([]byte("ok"))
		return
	}
	if m.To != text(s.config["corpid"]) {
		writeAPIError(w, 403, 403, "企业消息接收方无效")
		return
	}
	// Preserve the original source document privately, without forwarding the
	// encrypted envelope, callback credentials or unknown XML fields.
	private, _ := json.Marshal(map[string]any{"message_id": id, "user": m.From, "created": m.Created, "content": content, "agent": m.Agent})
	if e = api.incoming(r.Context(), c, s, id, m.From, m.From, content, time.Unix(m.Created, 0), private); e != nil {
		writeAPIError(w, 403, 403, "消息权限或原执行范围无效")
		return
	}
	_, _ = w.Write([]byte("ok"))
}
