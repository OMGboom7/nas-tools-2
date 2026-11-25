package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	appconfig "github.com/0xforee/nas-tools/backend/internal/config"
)

type telegramMedia struct {
	Title, URL, Type, Vote string
}

type telegramMessage struct {
	Token                   string
	ChatID                  string `json:"chat_id"`
	ThreadID                string `json:"thread_id"`
	Title, Text, Image, URL string
	Medias                  []telegramMedia
}

// SendTelegramCommand uses the same delivery path as native notifications.
// The bot token and the selected chat travel only on stdin.
func SendTelegramCommand(ctx context.Context, configPath, kind string, in io.Reader) error {
	var q telegramMessage
	r := &io.LimitedReader{R: in, N: 8 << 20}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if dec.Decode(&q) != nil || dec.Decode(new(any)) != io.EOF || r.N == 0 {
		return errors.New("invalid Telegram message")
	}
	var caption string
	switch kind {
	case "single":
		if q.Title == "" && q.Text == "" {
			return errors.New("empty Telegram message")
		}
		caption = telegramCaption(q.Title, q.Text, q.Image, q.URL)
	case "list":
		if q.Title == "" || q.Medias == nil {
			return errors.New("invalid Telegram list")
		}
		var body strings.Builder
		fmt.Fprintf(&body, "*%s*", q.Title)
		for i, media := range q.Medias {
			fmt.Fprintf(&body, "\n%d. [%s](%s)\n%s", i+1, media.Title, media.URL, media.Type)
			if media.Vote != "" {
				body.WriteString("，" + media.Vote)
			}
		}
		caption = body.String()
	default:
		return errors.New("invalid Telegram message kind")
	}
	service := notificationService{client: http.DefaultClient, config: appconfig.NewStore(configPath)}
	sent, err := service.deliverTelegram(ctx, q.Token, q.ChatID, q.ThreadID, caption, q.Image)
	if err != nil {
		return err
	}
	if !sent {
		return errors.New("Telegram rejected notification")
	}
	return nil
}

func (service notificationService) sendNativeTelegram(ctx context.Context, config map[string]any, title, message, image, clickURL string) (bool, error) {
	return service.deliverTelegram(ctx, text(config["token"]), text(config["chat_id"]), text(config["thread_id"]), telegramCaption(title, message, image, clickURL), image)
}

func telegramCaption(title, message, image, clickURL string) string {
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
	return caption
}

func (service notificationService) deliverTelegram(ctx context.Context, token, chatID, threadID, caption, image string) (bool, error) {
	token, chatID, threadID = strings.TrimSpace(token), strings.TrimSpace(chatID), strings.TrimSpace(threadID)
	if !validTelegramToken(token) || chatID == "" || len(chatID) > 256 || strings.ContainsAny(chatID, "\r\n") {
		return false, errors.New("notification channel is not configured")
	}
	base := "https://api.telegram.org"
	var proxies map[string]any
	if service.config != nil {
		snapshot, err := service.config.Snapshot()
		if err != nil {
			return false, errors.New("notification configuration is unavailable")
		}
		proxies = objectValue(objectValue(snapshot["app"])["proxies"])
		if d := strings.TrimSpace(text(objectValue(snapshot["laboratory"])["telegram_domain"])); d != "" {
			base = strings.TrimRight(d, "/")
		}
	}
	if len(base) > 2048 || !validHTTPURL(base) {
		return false, errors.New("notification channel is not configured")
	}
	endpoint := base + "/bot" + token
	client, err := service.telegramClient(base, proxies)
	if err != nil {
		return false, err
	}
	values := url.Values{"chat_id": {chatID}, "parse_mode": {"Markdown"}}
	if threadID != "" {
		values.Set("message_thread_id", threadID)
	}
	if image != "" {
		values.Set("photo", image)
		values.Set("caption", caption)
		sent, err := sendTelegramGET(ctx, &client, endpoint+"/sendPhoto?"+values.Encode())
		if sent || err != nil {
			return sent, err
		}
		values.Del("photo")
		values.Del("caption")
		if validHTTPURL(image) {
			imageClient, err := service.telegramClient(image, proxies)
			if err != nil {
				return false, err
			}
			if photo, ok := fetchTelegramPhoto(ctx, &imageClient, image); ok {
				sent, err = sendTelegramPhotoUpload(ctx, &client, endpoint+"/sendPhoto", values, caption, photo)
				if sent || err != nil {
					return sent, err
				}
			}
		}
	}
	values.Set("text", caption)
	return sendTelegramGET(ctx, &client, endpoint+"/sendMessage?"+values.Encode())
}

func (service notificationService) telegramClient(target string, proxies map[string]any) (http.Client, error) {
	client := service.notificationHTTPClient()
	parsed, err := url.Parse(target)
	if err != nil {
		return client, errors.New("invalid Telegram destination")
	}
	if strings.TrimSpace(text(proxies[parsed.Scheme])) != "" {
		client.Transport, err = siteProxyTransport(client.Transport, proxies, parsed.Scheme)
		if err != nil {
			return client, errors.New("notification proxy is unavailable")
		}
	} else {
		transport := client.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		if original, ok := transport.(*http.Transport); ok {
			client.Transport = original.Clone()
		}
	}
	// The Bot API accepts sends via GET. A reused connection can make Go
	// replay a GET after a lost response even though it created a message.
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.DisableKeepAlives = true
	}
	return client, nil
}

func sendTelegramGET(ctx context.Context, client *http.Client, endpoint string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, errors.New("invalid Telegram request")
	}
	return sendTelegramRequest(client, req)
}

// An unconfirmed photo submission must not trigger a second delivery.
func sendTelegramRequest(client *http.Client, request *http.Request) (bool, error) {
	response, err := client.Do(request)
	if err == nil {
		defer response.Body.Close()
		var result struct {
			OK *bool `json:"ok"`
		}
		const maxReceipt = 64 << 10
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxReceipt+1))
		if readErr == nil && len(body) <= maxReceipt && json.Unmarshal(body, &result) == nil && result.OK != nil {
			if response.StatusCode == http.StatusOK {
				return *result.OK, nil
			}
			if response.StatusCode >= 400 && response.StatusCode < 500 && !*result.OK {
				return false, nil
			}
		}
	}
	return false, errors.New("Telegram delivery receipt unavailable")
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

func sendTelegramPhotoUpload(ctx context.Context, client *http.Client, endpoint string, values url.Values, caption string, photo []byte) (bool, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, items := range values {
		for _, value := range items {
			if writer.WriteField(key, value) != nil {
				return false, errors.New("cannot prepare Telegram photo")
			}
		}
	}
	if writer.WriteField("caption", caption) != nil {
		return false, errors.New("cannot prepare Telegram photo")
	}
	part, err := writer.CreateFormFile("photo", "photo.jpg")
	if err != nil {
		return false, errors.New("cannot prepare Telegram photo")
	}
	if _, err := part.Write(photo); err != nil || writer.Close() != nil {
		return false, errors.New("cannot prepare Telegram photo")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return false, errors.New("cannot prepare Telegram photo")
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
