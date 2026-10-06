package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func nativeNotificationDeliverySupported(channelType string) bool {
	switch strings.ToLower(channelType) {
	case "serverchan", "ntfy", "gotify", "bark", "chanify", "iyuu", "pushplus", "pushdeer", "telegram", "synologychat", "wechat", "slack":
		return true
	}
	return false
}

func nativeNotificationDeliveryEligible(channelType string, config map[string]any) bool {
	if strings.EqualFold(channelType, "webhook") {
		return text(config["json_tpl"]) == ""
	}
	return nativeNotificationDeliverySupported(channelType)
}

func (service notificationService) sendNativeNotification(ctx context.Context, channelType string, config map[string]any, title, message, image, clickURL string) (bool, error) {
	switch strings.ToLower(channelType) {
	case "serverchan":
		return service.sendNativeServerChan(ctx, config, title, message)
	case "ntfy":
		return service.sendNativeNtfy(ctx, config, title, message)
	case "gotify":
		return service.sendNativeGotify(ctx, config, title, message, clickURL)
	case "bark":
		return service.sendNativeBark(ctx, config, title, message)
	case "chanify":
		return service.sendNativeChanify(ctx, config, title, message)
	case "iyuu":
		return service.sendNativeIyuu(ctx, config, title, message)
	case "pushplus":
		return service.sendNativePushPlus(ctx, config, title, message)
	case "pushdeer":
		return service.sendNativePushDeer(ctx, config, title, message)
	case "telegram":
		return service.sendNativeTelegram(ctx, config, title, message, image, clickURL)
	case "synologychat":
		return service.sendNativeSynologyChat(ctx, config, title, message, image, clickURL)
	case "webhook":
		return service.sendNativeWebhook(ctx, config, title, message, image, clickURL)
	case "wechat":
		return service.sendNativeWeChat(ctx, config, title, message, image, clickURL)
	case "slack":
		return service.sendNativeSlack(ctx, config, title, message, image, clickURL)
	}
	return false, errors.New("notification channel is unsupported")
}

func (service notificationService) notificationHTTPClient() http.Client {
	client := *service.client
	client.Timeout = 15 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func (service notificationService) sendNativeServerChan(ctx context.Context, config map[string]any, title, message string) (bool, error) {
	key := text(config["sckey"])
	if !validServerChanKey(key) {
		return false, errors.New("notification channel is not configured")
	}
	endpoint := url.URL{Scheme: "https", Host: "sctapi.ftqq.com", Path: "/" + key + ".send"}
	query := url.Values{"title": {title}, "desp": {message}}
	endpoint.RawQuery = query.Encode()
	return service.sendNotificationCode(ctx, http.MethodGet, endpoint.String(), "code", 0)
}

func (service notificationService) sendNativeNtfy(ctx context.Context, config map[string]any, title, message string) (bool, error) {
	server, err := notificationServerRoot(text(config["server"]))
	if err != nil {
		return false, err
	}
	token, topic := strings.TrimSpace(text(config["token"])), strings.TrimSpace(text(config["topic"]))
	if token == "" || topic == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n") {
		return false, errors.New("notification channel is not configured")
	}
	priority := notificationPriority(config["priority"], 4)
	tags := text(config["tags"])
	if tags == "" {
		tags = "rotating_light"
	}
	data := map[string]any{
		"topic": topic, "title": title, "message": message, "priority": priority, "tags": strings.Split(tags, ","),
	}
	return service.sendNotificationJSON(ctx, server, "Bearer "+token, data)
}

func (service notificationService) sendNativeGotify(ctx context.Context, config map[string]any, title, message, clickURL string) (bool, error) {
	server, err := notificationServerRoot(text(config["server"]))
	if err != nil {
		return false, err
	}
	token := strings.TrimSpace(text(config["token"]))
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n") {
		return false, errors.New("notification channel is not configured")
	}
	server.Path = "/message"
	server.RawQuery = url.Values{"token": {token}}.Encode()
	data := map[string]any{
		"title": title, "message": message, "priority": notificationPriority(config["priority"], 8),
		"extras": map[string]any{"client::notification": map[string]any{"click": map[string]any{"url": clickURL}}},
	}
	return service.sendNotificationJSON(ctx, server, "", data)
}

func (service notificationService) sendNativeBark(ctx context.Context, config map[string]any, title, message string) (bool, error) {
	server, err := notificationServerRoot(text(config["server"]))
	if err != nil {
		return false, err
	}
	key := strings.TrimSpace(text(config["apikey"]))
	if !validServerChanKey(key) {
		return false, errors.New("notification channel is not configured")
	}
	query, err := url.ParseQuery(text(config["params"]))
	if err != nil {
		return false, errors.New("notification channel is not configured")
	}
	endpoint := server.Scheme + "://" + server.Host + "/" + url.PathEscape(key) + "/" + url.QueryEscape(title) + "/" + url.QueryEscape(message)
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	return service.sendNotificationCode(ctx, http.MethodPost, endpoint, "code", 200)
}

func (service notificationService) sendNativeIyuu(ctx context.Context, config map[string]any, title, message string) (bool, error) {
	token := strings.TrimSpace(text(config["token"]))
	if !validServerChanKey(token) {
		return false, errors.New("notification channel is not configured")
	}
	endpoint := url.URL{Scheme: "http", Host: "iyuu.cn", Path: "/" + token + ".send"}
	endpoint.RawQuery = url.Values{"text": {title}, "desp": {message}}.Encode()
	return service.sendNotificationCode(ctx, http.MethodGet, endpoint.String(), "errcode", 0)
}

func (service notificationService) sendNativePushPlus(ctx context.Context, config map[string]any, title, message string) (bool, error) {
	token, channel := strings.TrimSpace(text(config["token"])), strings.TrimSpace(text(config["channel"]))
	if token == "" || channel == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n") {
		return false, errors.New("notification channel is not configured")
	}
	if message == "" {
		message = "无"
	}
	query := url.Values{
		"token": {token}, "channel": {channel}, "topic": {text(config["topic"])},
		"webhook": {text(config["webhook"])}, "title": {title}, "content": {message},
		"timestamp": {strconv.FormatInt(time.Now().UnixNano()+60, 10)},
	}
	endpoint := url.URL{Scheme: "http", Host: "www.pushplus.plus", Path: "/send", RawQuery: query.Encode()}
	return service.sendNotificationCode(ctx, http.MethodGet, endpoint.String(), "code", 200)
}

func (service notificationService) sendNativePushDeer(ctx context.Context, config map[string]any, title, message string) (bool, error) {
	server, err := notificationServerRoot(text(config["server"]))
	if err != nil {
		return false, err
	}
	key := strings.TrimSpace(text(config["apikey"]))
	if key == "" || len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
		return false, errors.New("notification channel is not configured")
	}
	server.Path = "/message/push"
	values := url.Values{"pushkey": {key}, "text": {title}, "desp": {message}, "type": {"markdown"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.String(), strings.NewReader(values.Encode()))
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := service.notificationHTTPClient()
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, nil
	}
	var result struct {
		Code *int `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return false, nil
	}
	return result.Code != nil && *result.Code == 0, nil
}

func (service notificationService) sendNativeTelegram(ctx context.Context, config map[string]any, title, message, image, clickURL string) (bool, error) {
	token := strings.TrimSpace(text(config["token"]))
	chatID := strings.TrimSpace(text(config["chat_id"]))
	if !validTelegramToken(token) || chatID == "" || len(chatID) > 256 || strings.ContainsAny(chatID, "\r\n") {
		return false, errors.New("notification channel is not configured")
	}
	base := "https://api.telegram.org"
	var app map[string]any
	if service.config != nil {
		snapshot, err := service.config.Snapshot()
		if err != nil {
			return false, errors.New("notification configuration is unavailable")
		}
		app = objectValue(snapshot["app"])
		if domain := strings.TrimSpace(text(objectValue(snapshot["laboratory"])["telegram_domain"])); domain != "" {
			base = strings.TrimRight(domain, "/")
		}
	}
	if len(base) > 2048 || !validHTTPURL(base) {
		return false, errors.New("notification channel is not configured")
	}
	parsed, _ := url.Parse(base)
	transport := service.client.Transport
	if proxy := strings.TrimSpace(text(objectValue(app["proxies"])[parsed.Scheme])); proxy != "" {
		configured, err := siteProxyTransport(transport, map[string]any{parsed.Scheme: proxy}, parsed.Scheme)
		if err != nil {
			return false, errors.New("notification proxy is unavailable")
		}
		transport = configured
	}
	firstTitle, continuation, hasContinuation := strings.Cut(title, "\n")
	message = strings.NewReplacer("[", `\[`, "_", `\_`, "*", `\*`, "`", "\\`").Replace(message)
	if hasContinuation {
		if message != "" {
			message = continuation + "\n" + message
		} else {
			message = continuation
		}
		title = firstTitle
	}
	caption := title
	if message != "" {
		caption = "*" + title + "*\n" + strings.ReplaceAll(message, "\n\n", "\n")
	}
	if image != "" && clickURL != "" {
		caption += "\n\n[查看详情](" + clickURL + ")"
	}
	endpoint := strings.TrimRight(base, "/") + "/bot" + token
	values := url.Values{"chat_id": {chatID}, "parse_mode": {"Markdown"}}
	if threadID := strings.TrimSpace(text(config["thread_id"])); threadID != "" {
		values.Set("message_thread_id", threadID)
	}
	scoped := service
	scoped.client = &http.Client{Transport: transport}
	client := scoped.notificationHTTPClient()
	if image != "" {
		photoValues := url.Values{}
		for key, items := range values {
			photoValues[key] = append([]string(nil), items...)
		}
		photoValues.Set("photo", image)
		photoValues.Set("caption", caption)
		if sendTelegramGET(ctx, &client, endpoint+"/sendPhoto?"+photoValues.Encode()) {
			return true, nil
		}
		if photo, ok := fetchTelegramPhoto(ctx, &client, image); ok && sendTelegramPhotoUpload(ctx, &client, endpoint+"/sendPhoto", values, caption, photo) {
			return true, nil
		}
	}
	values.Set("text", caption)
	return sendTelegramGET(ctx, &client, endpoint+"/sendMessage?"+values.Encode()), nil
}

func sendTelegramGET(ctx context.Context, client *http.Client, endpoint string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	return sendTelegramRequest(client, request)
}

func sendTelegramRequest(client *http.Client, request *http.Request) bool {
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return false
	}
	return result.OK
}

func fetchTelegramPhoto(ctx context.Context, client *http.Client, image string) ([]byte, bool) {
	if len(image) > 2048 || !validHTTPURL(image) {
		return nil, false
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, image, nil)
	if err != nil {
		return nil, false
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, false
	}
	const maxPhotoSize = 10 << 20
	photo, err := io.ReadAll(io.LimitReader(response.Body, maxPhotoSize+1))
	return photo, err == nil && len(photo) > 0 && len(photo) <= maxPhotoSize
}

func sendTelegramPhotoUpload(ctx context.Context, client *http.Client, endpoint string, values url.Values, caption string, photo []byte) bool {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, items := range values {
		for _, value := range items {
			if writer.WriteField(key, value) != nil {
				return false
			}
		}
	}
	if writer.WriteField("caption", caption) != nil {
		return false
	}
	part, err := writer.CreateFormFile("photo", "photo.jpg")
	if err != nil {
		return false
	}
	if _, err := part.Write(photo); err != nil || writer.Close() != nil {
		return false
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return false
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return sendTelegramRequest(client, request)
}

func validTelegramToken(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == ':' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func (service notificationService) sendNativeSynologyChat(ctx context.Context, config map[string]any, title, message, image, clickURL string) (bool, error) {
	webhookURL := strings.TrimSpace(text(config["webhook_url"]))
	token := strings.TrimSpace(text(config["token"]))
	if len(webhookURL) > 2048 || !validHTTPURL(webhookURL) || token == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n") {
		return false, errors.New("notification channel is not configured")
	}
	parsed, _ := url.Parse(webhookURL)
	userList := url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: "/webapi/entry.cgi"}
	userList.RawQuery = url.Values{
		"api": {"SYNO.Chat.External"}, "method": {"user_list"}, "version": {"2"}, "token": {token},
	}.Encode()
	client := service.notificationHTTPClient()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, userList.String(), nil)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	var users struct {
		Data struct {
			Users []struct {
				ID json.Number `json:"user_id"`
			} `json:"users"`
		} `json:"data"`
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return false, nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	decoder.UseNumber()
	err = decoder.Decode(&users)
	_ = response.Body.Close()
	if err != nil || len(users.Data.Users) == 0 || len(users.Data.Users) > 1000 {
		return false, nil
	}
	firstTitle, continuation, hasContinuation := strings.Cut(title, "\n")
	if hasContinuation {
		if message != "" {
			message = continuation + "\n" + message
		} else {
			message = continuation
		}
		title = firstTitle
	}
	caption := title
	if message != "" {
		caption = "*" + title + "*\n" + strings.ReplaceAll(message, "\n\n", "\n")
	}
	if image != "" && clickURL != "" {
		caption += "\n\n<" + clickURL + "|查看详情>"
	}
	// The old Synology client percent-encodes the caption before JSON encoding.
	encodedCaption := strings.ReplaceAll(url.PathEscape(caption), "%2F", "/")
	for _, user := range users.Data.Users {
		id, err := strconv.ParseInt(user.ID.String(), 10, 64)
		if err != nil || id < 1 {
			return false, nil
		}
		payload, err := json.Marshal(map[string]any{"text": encodedCaption, "user_ids": []int64{id}})
		if err != nil {
			return false, errors.New("notification delivery failed")
		}
		form := url.Values{"payload": {string(payload)}}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, strings.NewReader(form.Encode()))
		if err != nil {
			return false, errors.New("notification delivery failed")
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(request)
		if err != nil {
			return false, errors.New("notification delivery failed")
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return false, nil
		}
		var result map[string]any
		err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result)
		_ = response.Body.Close()
		if err != nil || len(result) == 0 || number(objectValue(result["error"])["code"]) != 0 {
			return false, nil
		}
	}
	return true, nil
}

func (service notificationService) sendNativeWebhook(ctx context.Context, config map[string]any, title, message, image, clickURL string) (bool, error) {
	if text(config["json_tpl"]) != "" {
		return false, errors.New("notification template is unsupported")
	}
	rawURL := strings.TrimSpace(text(config["url"]))
	method := strings.ToUpper(strings.TrimSpace(text(config["method"])))
	if len(rawURL) > 2048 || !validHTTPURL(rawURL) {
		return false, errors.New("notification channel is not configured")
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return false, errors.New("notification channel is not configured")
	}
	parsed, _ := url.Parse(rawURL)
	query := parsed.Query()
	if raw := strings.TrimSpace(text(config["query_params"])); raw != "" {
		var extra map[string]any
		if err := json.Unmarshal([]byte(raw), &extra); err != nil {
			return false, errors.New("notification channel is not configured")
		}
		for key, value := range extra {
			if key == "" {
				continue
			}
			switch item := value.(type) {
			case nil:
				continue
			case string, float64:
				query.Add(key, text(item))
			case bool:
				if item {
					query.Add(key, "True")
				} else {
					query.Add(key, "False")
				}
			case []any:
				for _, element := range item {
					if element == nil {
						continue
					}
					if _, nested := element.(map[string]any); nested {
						return false, errors.New("notification channel is not configured")
					}
					query.Add(key, text(element))
				}
			default:
				return false, errors.New("notification channel is not configured")
			}
		}
	}
	parsed.RawQuery = query.Encode()
	body, err := json.Marshal(map[string]any{
		"title": title, "text": message, "image": image, "url": clickURL, "user_id": "",
	})
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	request, err := http.NewRequestWithContext(ctx, method, parsed.String(), bytes.NewReader(body))
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	request.Header.Set("Content-Type", "application/json")
	if token := text(config["token"]); token != "" {
		if len(token) > 4096 || strings.ContainsAny(token, "\r\n") {
			return false, errors.New("notification channel is not configured")
		}
		request.Header.Set("Authorization", token)
	}
	client := service.notificationHTTPClient()
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	defer response.Body.Close()
	return response.StatusCode >= 200 && response.StatusCode < 300, nil
}

func (service notificationService) sendNativeWeChat(ctx context.Context, config map[string]any, title, message, image, clickURL string) (bool, error) {
	corpID := strings.TrimSpace(text(config["corpid"]))
	corpSecret := strings.TrimSpace(text(config["corpsecret"]))
	agentID := strings.TrimSpace(text(config["agentid"]))
	if corpID == "" || corpSecret == "" || agentID == "" || len(corpID) > 1024 || len(corpSecret) > 4096 || len(agentID) > 256 {
		return false, errors.New("notification channel is not configured")
	}
	base := "https://qyapi.weixin.qq.com"
	if proxy := strings.TrimSpace(text(config["default_proxy"])); proxy != "" {
		if len(proxy) > 2048 || !validHTTPURL(proxy) {
			return false, errors.New("notification channel is not configured")
		}
		base = strings.TrimRight(proxy, "/")
	}
	client := service.notificationHTTPClient()
	tokenURL := base + "/cgi-bin/gettoken?" + url.Values{"corpid": {corpID}, "corpsecret": {corpSecret}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	var tokenResult struct {
		ErrCode     *int   `json:"errcode"`
		AccessToken string `json:"access_token"`
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return false, nil
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&tokenResult)
	_ = response.Body.Close()
	if err != nil || tokenResult.ErrCode == nil || *tokenResult.ErrCode != 0 || tokenResult.AccessToken == "" || len(tokenResult.AccessToken) > 4096 {
		return false, nil
	}
	sendURL := base + "/cgi-bin/message/send?" + url.Values{"access_token": {tokenResult.AccessToken}}.Encode()
	for index, chunk := range splitNotificationText(message, 2048) {
		chunkTitle := title
		if index > 0 {
			chunkTitle = ""
		}
		var payload map[string]any
		if image != "" {
			payload = map[string]any{
				"touser": "@all", "msgtype": "news", "agentid": agentID,
				"news": map[string]any{"articles": []any{map[string]any{
					"title": chunkTitle, "description": strings.ReplaceAll(chunk, "\n\n", "\n"), "picurl": image, "url": clickURL,
				}}},
			}
		} else {
			content := chunkTitle
			if chunk != "" {
				if content != "" {
					content += "\n"
				}
				content += strings.ReplaceAll(chunk, "\n\n", "\n")
			}
			if clickURL != "" {
				content += "\n\n<a href='" + clickURL + "'>查看详情</a>"
			}
			payload = map[string]any{
				"touser": "@all", "msgtype": "text", "agentid": agentID,
				"text": map[string]any{"content": content}, "safe": 0,
				"enable_id_trans": 0, "enable_duplicate_check": 0,
			}
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return false, errors.New("notification delivery failed")
		}
		request, err = http.NewRequestWithContext(ctx, http.MethodPost, sendURL, bytes.NewReader(body))
		if err != nil {
			return false, errors.New("notification delivery failed")
		}
		request.Header.Set("Content-Type", "application/json")
		response, err = client.Do(request)
		if err != nil {
			return false, errors.New("notification delivery failed")
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return false, nil
		}
		var result struct {
			ErrCode *int `json:"errcode"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result)
		_ = response.Body.Close()
		if err != nil || result.ErrCode == nil || *result.ErrCode != 0 {
			return false, nil
		}
	}
	return true, nil
}

func splitNotificationText(value string, maxBytes int) []string {
	if value == "" {
		return []string{""}
	}
	chunks := []string{}
	start, size := 0, 0
	for index, char := range value {
		width := utf8.RuneLen(char)
		if size+width > maxBytes {
			chunks = append(chunks, value[start:index])
			start, size = index, 0
		}
		size += width
	}
	return append(chunks, value[start:])
}

func (service notificationService) sendNativeSlack(ctx context.Context, config map[string]any, title, message, image, clickURL string) (bool, error) {
	token := strings.TrimSpace(text(config["bot_token"]))
	channelName := strings.TrimSpace(text(config["channel"]))
	if channelName == "" {
		channelName = "全体"
	}
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n") || len(channelName) > 256 {
		return false, errors.New("notification channel is not configured")
	}
	client := service.notificationHTTPClient()
	channelID, err := service.findSlackChannel(ctx, &client, token, channelName)
	if err != nil || channelID == "" {
		return false, err
	}
	firstTitle, continuation, hasContinuation := strings.Cut(title, "\n")
	if hasContinuation {
		title = firstTitle
		if message == "" {
			message = continuation
		} else {
			message = continuation + "\n" + message
		}
	}
	section := map[string]any{
		"type": "section",
		"text": map[string]any{"type": "mrkdwn", "text": "*" + title + "*\n" + message},
	}
	if image != "" {
		section["accessory"] = map[string]any{"type": "image", "image_url": image, "alt_text": title}
	}
	blocks := []any{section}
	if image != "" && clickURL != "" {
		blocks = append(blocks, map[string]any{
			"type": "actions", "elements": []any{map[string]any{
				"type": "button", "text": map[string]any{"type": "plain_text", "text": "查看详情", "emoji": true},
				"value": "click_me_url", "url": clickURL, "action_id": "actionId-url",
			}},
		})
	}
	return service.slackAPI(ctx, &client, http.MethodPost, "chat.postMessage", token, map[string]any{
		"channel": channelID, "blocks": blocks,
	})
}

func (service notificationService) findSlackChannel(ctx context.Context, client *http.Client, token, channelName string) (string, error) {
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		query := url.Values{"limit": {"200"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://slack.com/api/conversations.list?"+query.Encode(), nil)
		if err != nil {
			return "", errors.New("notification delivery failed")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			return "", errors.New("notification delivery failed")
		}
		var result struct {
			OK       bool `json:"ok"`
			Channels []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"channels"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || decodeErr != nil || !result.OK {
			return "", nil
		}
		for _, channel := range result.Channels {
			if channel.Name == channelName && channel.ID != "" {
				return channel.ID, nil
			}
		}
		cursor = result.ResponseMetadata.NextCursor
		if cursor == "" {
			return "", nil
		}
		if len(cursor) > 4096 || seen[cursor] {
			return "", nil
		}
		seen[cursor] = true
	}
	return "", nil
}

func (service notificationService) slackAPI(ctx context.Context, client *http.Client, method, name, token string, payload any) (bool, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	request, err := http.NewRequestWithContext(ctx, method, "https://slack.com/api/"+name, bytes.NewReader(body))
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, nil
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return false, nil
	}
	return result.OK, nil
}

func (service notificationService) sendNotificationCode(ctx context.Context, method, endpoint, field string, successCode int) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	client := service.notificationHTTPClient()
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, nil
	}
	var result map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return false, nil
	}
	var code *int
	if err := json.Unmarshal(result[field], &code); err != nil {
		return false, nil
	}
	return code != nil && *code == successCode, nil
}

func (service notificationService) sendNativeChanify(ctx context.Context, config map[string]any, title, message string) (bool, error) {
	server, err := notificationServerRoot(text(config["server"]))
	if err != nil {
		return false, err
	}
	token := strings.TrimSpace(text(config["token"]))
	if !validServerChanKey(token) {
		return false, errors.New("notification channel is not configured")
	}
	values, err := url.ParseQuery(text(config["params"]))
	if err != nil {
		return false, errors.New("notification channel is not configured")
	}
	values.Set("title", title)
	values.Set("text", message)
	server.Path = "/v1/sender/" + token
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.String(), strings.NewReader(values.Encode()))
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := service.notificationHTTPClient()
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK, nil
}

func (service notificationService) sendNotificationJSON(ctx context.Context, endpoint url.URL, authorization string, data any) (bool, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	client := service.notificationHTTPClient()
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("notification delivery failed")
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK, nil
}

func notificationServerRoot(raw string) (url.URL, error) {
	if len(raw) > 2048 || !validHTTPURL(raw) {
		return url.URL{}, errors.New("notification channel is not configured")
	}
	parsed, _ := url.Parse(strings.TrimSpace(raw))
	return url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: "/"}, nil
}

func notificationPriority(value any, fallback int) int {
	priority, err := strconv.Atoi(strings.TrimSpace(text(value)))
	if err != nil {
		return fallback
	}
	return priority
}

func validServerChanKey(key string) bool {
	if len(key) == 0 || len(key) > 256 {
		return false
	}
	for index := 0; index < len(key); index++ {
		char := key[index]
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_') {
			return false
		}
	}
	return true
}
