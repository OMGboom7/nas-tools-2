package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

func (service notificationService) sendNativeReply(ctx context.Context, kind string, config map[string]any, user, title, message string) (bool, error) {
	if user == "" || len(user) > 512 || strings.ContainsAny(user, "\x00\r\n") {
		return false, errors.New("invalid original reply recipient")
	}
	switch kind {
	case "telegram":
		if id, e := strconv.ParseInt(user, 10, 64); e != nil || id <= 0 {
			return false, errors.New("invalid original Telegram recipient")
		}
		copy := map[string]any{}
		for k, v := range config {
			copy[k] = v
		}
		copy["chat_id"] = user
		copy["thread_id"] = ""
		return service.sendNativeTelegram(ctx, copy, title, message, "", "")
	case "wechat":
		if user == "@all" || strings.ContainsAny(user, "|;, ") {
			return false, errors.New("invalid original WeChat recipient")
		}
		return service.sendNativeWeChatTo(ctx, config, title, message, "", "", user)
	case "slack":
		return service.sendNativeSlackTo(ctx, config, title, message, "", "", user)
	case "synologychat":
		id, e := strconv.ParseInt(user, 10, 64)
		if e != nil || id <= 0 {
			return false, errors.New("invalid original reply recipient")
		}
		address := text(config["webhook_url"])
		if !validHTTPURL(address) {
			return false, errors.New("invalid original reply endpoint")
		}
		caption := strings.ReplaceAll(url.PathEscape("*"+title+"*\n"+message), "%2F", "/")
		payload, _ := json.Marshal(map[string]any{"text": caption, "user_ids": []int64{id}})
		form := url.Values{"payload": {string(payload)}}
		request, e := http.NewRequestWithContext(ctx, "POST", address, strings.NewReader(form.Encode()))
		if e != nil {
			return false, e
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		client := service.notificationHTTPClient()
		response, e := client.Do(request)
		if e != nil {
			return false, e
		}
		defer response.Body.Close()
		var result struct {
			Success bool `json:"success"`
		}
		if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result) != nil {
			return false, nil
		}
		return result.Success, nil
	}
	return false, errors.New("unsupported original reply channel")
}
func (service notificationService) openSlackDirect(ctx context.Context, client *http.Client, token, user string) (string, error) {
	if !regexp.MustCompile(`^[UW][A-Z0-9]+$`).MatchString(user) {
		return "", errors.New("invalid original Slack recipient")
	}
	raw, _ := json.Marshal(map[string]string{"users": user})
	request, e := http.NewRequestWithContext(ctx, "POST", "https://slack.com/api/conversations.open", bytes.NewReader(raw))
	if e != nil {
		return "", e
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, e := client.Do(request)
	if e != nil {
		return "", e
	}
	defer response.Body.Close()
	var result struct {
		OK      bool `json:"ok"`
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result) != nil || !result.OK || result.Channel.ID == "" {
		return "", errors.New("original Slack direct channel not confirmed")
	}
	return result.Channel.ID, nil
}
