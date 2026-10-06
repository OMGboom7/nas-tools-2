package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
)

func parseLegacyNotificationForm(response http.ResponseWriter, request *http.Request) bool {
	if _, ok := requireServiceToken(response, request); !ok {
		return false
	}
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification request")
		return false
	}
	return true
}

func legacyNotificationID(raw string, allowEmpty bool) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if allowEmpty && (raw == "" || raw == "0") {
		return 0, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0
}

func legacyNotificationFlag(raw string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on":
		return 1, true
	case "0", "false", "off", "":
		return 0, true
	default:
		return 0, false
	}
}

func (service notificationService) serveLegacyUpdate(response http.ResponseWriter, request *http.Request) {
	if !parseLegacyNotificationForm(response, request) {
		return
	}
	id, ok := legacyNotificationID(request.Form.Get("cid"), true)
	name := strings.TrimSpace(request.Form.Get("name"))
	ctype := strings.ToLower(strings.TrimSpace(request.Form.Get("type")))
	interactive, interactiveOK := legacyNotificationFlag(request.Form.Get("interactive"))
	enabled, enabledOK := legacyNotificationFlag(request.Form.Get("enabled"))
	if !ok || name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n\t") || notificationTypeLabels[ctype] == "" || !interactiveOK || !enabledOK || interactive != 0 && !interactiveNotificationTypes[ctype] {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification channel")
		return
	}
	configuration := request.Form.Get("config")
	var parsed map[string]any
	if len(configuration) > 65536 || json.Unmarshal([]byte(configuration), &parsed) != nil || parsed == nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification configuration")
		return
	}
	events := request.Form["switchs"]
	if events == nil {
		events = []string{}
	}
	if len(events) == 1 && strings.HasPrefix(strings.TrimSpace(events[0]), "[") {
		if json.Unmarshal([]byte(events[0]), &events) != nil {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid notification events")
			return
		}
	}
	allowed := make(map[string]bool, len(notificationEvents))
	for _, event := range notificationEvents {
		allowed[event.ID] = true
	}
	if len(events) > len(notificationEvents) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification events")
		return
	}
	for _, event := range events {
		if !allowed[event] {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid notification events")
			return
		}
	}
	eventsJSON, _ := json.Marshal(events)
	_, err := service.store.Save(request.Context(), notificationconfig.Channel{
		ID: id, Name: name, Type: ctype, Config: configuration, Switches: string(eventsJSON),
		Interactive: interactive, Enabled: enabled,
	})
	if err != nil {
		writeLegacyNotificationMutationError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0})
}

func (service notificationService) serveLegacyStatus(response http.ResponseWriter, request *http.Request) {
	if !parseLegacyNotificationForm(response, request) {
		return
	}
	id, ok := legacyNotificationID(request.Form.Get("cid"), false)
	flag := request.Form.Get("flag")
	checked, checkedOK := legacyNotificationFlag(request.Form.Get("checked"))
	if !ok || !checkedOK || flag != "interactive" && flag != "enable" {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification status")
		return
	}
	if flag == "interactive" && checked != 0 {
		channel, err := service.store.Get(request.Context(), id)
		if err != nil {
			writeLegacyNotificationMutationError(response, err)
			return
		}
		if !interactiveNotificationTypes[channel.Type] {
			writeAPIError(response, http.StatusBadRequest, 400, "notification channel does not support interaction")
			return
		}
	}
	if err := service.store.SetStatus(request.Context(), id, flag == "interactive", checked != 0); err != nil {
		writeLegacyNotificationMutationError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0})
}

func (service notificationService) serveLegacyDelete(response http.ResponseWriter, request *http.Request) {
	if !parseLegacyNotificationForm(response, request) {
		return
	}
	id, ok := legacyNotificationID(request.Form.Get("cid"), false)
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification channel id")
		return
	}
	if err := service.store.Delete(request.Context(), id); err != nil {
		writeLegacyNotificationMutationError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0})
}

func (service notificationService) serveLegacyTest(response http.ResponseWriter, request *http.Request) {
	if !parseLegacyNotificationForm(response, request) {
		return
	}
	ctype := strings.ToLower(strings.TrimSpace(request.Form.Get("type")))
	configuration := request.Form.Get("config")
	var parsed map[string]any
	if len(configuration) > 65536 || json.Unmarshal([]byte(configuration), &parsed) != nil || parsed == nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification configuration")
		return
	}
	if !nativeNotificationDeliveryEligible(ctype, parsed) {
		writeAPIError(response, http.StatusNotImplemented, 501, "notification channel test is not migrated")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	sent, _ := service.sendNativeNotification(ctx, ctype, parsed, "测试", "这是一条测试消息", "", "https://github.com/0xforee/nas-tools")
	code := 1
	if sent {
		code = 0
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": code})
}

func (service notificationService) serveLegacyCustomMessage(response http.ResponseWriter, request *http.Request) {
	if !parseLegacyNotificationForm(response, request) {
		return
	}
	input := customMessageRequest{
		Title: strings.TrimSpace(request.Form.Get("title")), Text: request.Form.Get("text"),
		Image: strings.TrimSpace(request.Form.Get("image")), ChannelIDs: request.Form["message_clients"],
	}
	if input.Title == "" || len(input.Title) > 200 || strings.ContainsAny(input.Title, "\r\n\x00") || len(input.Text) > 10000 || strings.ContainsRune(input.Text, '\x00') || input.Image != "" && (len(input.Image) > 2048 || !validHTTPURL(input.Image)) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid custom message")
		return
	}
	ids, message := validateCustomMessageChannelIDs(input.ChannelIDs)
	if message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	service.deliverCustomMessage(response, ctx, request.Header.Get("Authorization"), input, ids, true)
}

func writeLegacyNotificationMutationError(response http.ResponseWriter, err error) {
	if errors.Is(err, notificationconfig.ErrNotFound) {
		writeAPIError(response, http.StatusNotFound, 404, "notification channel not found")
		return
	}
	writeAPIError(response, http.StatusBadGateway, 502, "notification channel could not be saved")
}
