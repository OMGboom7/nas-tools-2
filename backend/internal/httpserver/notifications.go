package httpserver

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
)

type notificationService struct {
	client *http.Client
	store  *notificationconfig.Store
	config *config.Store
}

type notificationChannel struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	TypeLabel    string   `json:"typeLabel"`
	Enabled      bool     `json:"enabled"`
	Interactive  bool     `json:"interactive"`
	CanInteract  bool     `json:"canInteract"`
	Configured   bool     `json:"configured"`
	Switches     []string `json:"switches"`
	SwitchLabels []string `json:"switchLabels"`
}

type notificationEvent struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type notificationsData struct {
	Items  []notificationChannel `json:"items"`
	Events []notificationEvent   `json:"events"`
}

type notificationFieldChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type notificationFieldOption struct {
	Key         string                    `json:"key"`
	Title       string                    `json:"title"`
	Type        string                    `json:"type"`
	Required    bool                      `json:"required"`
	Tooltip     string                    `json:"tooltip"`
	Placeholder string                    `json:"placeholder"`
	Default     any                       `json:"default"`
	Options     []notificationFieldChoice `json:"options"`
	WriteOnly   bool                      `json:"writeOnly"`
}

type notificationChannelOption struct {
	Type        string                    `json:"type"`
	Name        string                    `json:"name"`
	CanInteract bool                      `json:"canInteract"`
	Fields      []notificationFieldOption `json:"fields"`
}

type notificationOptionsData struct {
	Channels []notificationChannelOption `json:"channels"`
	Events   []notificationEvent         `json:"events"`
}

type notificationDetail struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Type             string         `json:"type"`
	Enabled          bool           `json:"enabled"`
	Interactive      bool           `json:"interactive"`
	CanInteract      bool           `json:"canInteract"`
	Events           []string       `json:"events"`
	Config           map[string]any `json:"config"`
	ConfiguredFields []string       `json:"configuredFields"`
}

type notificationConfigRequest struct {
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Enabled     bool           `json:"enabled"`
	Interactive bool           `json:"interactive"`
	Events      []string       `json:"events"`
	Config      map[string]any `json:"config"`
	ClearConfig []string       `json:"clearConfig"`
}

type notificationStatusRequest struct {
	Enabled     *bool `json:"enabled"`
	Interactive *bool `json:"interactive"`
}

type notificationTestData struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Duration int64  `json:"duration"`
	Message  string `json:"message"`
}

type customMessageRequest struct {
	Title      string   `json:"title"`
	Text       string   `json:"text"`
	Image      string   `json:"image"`
	ChannelIDs []string `json:"channelIds"`
}

type customMessageData struct {
	ChannelCount int `json:"channelCount"`
}

var notificationEvents = []notificationEvent{
	{ID: "download_start", Label: "新增下载"}, {ID: "download_fail", Label: "下载失败"},
	{ID: "transfer_finished", Label: "入库完成"}, {ID: "transfer_fail", Label: "入库失败"},
	{ID: "rss_added", Label: "新增订阅"}, {ID: "rss_finished", Label: "订阅完成"},
	{ID: "site_signin", Label: "站点签到"}, {ID: "site_message", Label: "站点消息"},
	{ID: "brushtask_added", Label: "刷流下种"}, {ID: "brushtask_remove", Label: "刷流删种"},
	{ID: "auto_remove_torrents", Label: "自动删种"}, {ID: "ptrefresh_date_message", Label: "数据统计"},
	{ID: "mediaserver_message", Label: "媒体服务"}, {ID: "custom_message", Label: "插件消息"},
}

var notificationTypeLabels = map[string]string{
	"telegram": "Telegram", "wechat": "微信", "serverchan": "Server酱", "bark": "Bark",
	"pushdeer": "PushDeer", "pushplus": "PushPlus", "iyuu": "爱语飞飞", "slack": "Slack",
	"gotify": "Gotify", "ntfy": "ntfy", "chanify": "Chanify", "synologychat": "Synology Chat",
	"webhook": "Webhook",
}

var interactiveNotificationTypes = map[string]bool{"telegram": true, "wechat": true, "slack": true, "synologychat": true}

// notification_options.json preserves the fields and order from ModuleConf.MESSAGE_CONF.
//
//go:embed notification_options.json
var nativeNotificationOptionsJSON []byte

func (service notificationService) serveList(response http.ResponseWriter, request *http.Request) {
	if !service.requireStore(response) {
		return
	}
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	result, err := service.channelRecords(ctx, token)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "notification channels are unavailable")
		return
	}
	if number(result["code"]) == 403 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	if number(result["code"]) != 0 {
		writeAPIError(response, http.StatusBadGateway, 502, "notification channels are unavailable")
		return
	}
	data := notificationsData{Items: normalizeNotificationChannels(legacyPayload(result)["detail"]), Events: notificationEvents}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func (service notificationService) serveLegacyInfo(response http.ResponseWriter, request *http.Request) {
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification channel request")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	cid := strings.TrimSpace(request.Form.Get("cid"))
	if cid != "" && cid != "0" {
		id, err := strconv.ParseInt(cid, 10, 64)
		if err != nil || id < 1 {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid notification channel id")
			return
		}
		item, err := service.store.Get(ctx, id)
		if errors.Is(err, notificationconfig.ErrNotFound) {
			writeJSON(response, http.StatusOK, map[string]any{"code": 0, "detail": nil})
			return
		}
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "notification channels are unavailable")
			return
		}
		record, err := nativeNotificationRecord(item)
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "notification configuration is invalid")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "detail": record})
		return
	}
	result, err := service.channelRecords(ctx, token)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "notification channels are unavailable")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "detail": legacyPayload(result)["detail"]})
}

func (service notificationService) serveOptions(response http.ResponseWriter, request *http.Request) {
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	data, result, err := service.options(ctx, token)
	if !handleNotificationOptionsResult(response, data, result, err) {
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func (service notificationService) serveLegacyOptions(response http.ResponseWriter, request *http.Request) {
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	data, _, err := service.options(request.Context(), token)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "notification options are unavailable")
		return
	}
	channels := make([]any, 0, len(data.Channels))
	for _, channel := range data.Channels {
		fields := make([]any, 0, len(channel.Fields))
		for _, field := range channel.Fields {
			fields = append(fields, map[string]any{
				"key": field.Key, "title": field.Title, "type": field.Type, "required": field.Required,
				"tooltip": field.Tooltip, "placeholder": field.Placeholder, "default": field.Default,
				"options": field.Options, "write_only": field.WriteOnly,
			})
		}
		channels = append(channels, map[string]any{
			"type": channel.Type, "name": channel.Name, "can_interact": channel.CanInteract, "fields": fields,
		})
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "channels": channels, "events": data.Events})
}

func (service notificationService) serveDetail(response http.ResponseWriter, request *http.Request) {
	if !service.requireStore(response) {
		return
	}
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id, ok := validServiceID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification channel id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	raw, result, err := service.detail(ctx, token, id)
	if !handleNotificationDetailResult(response, raw, result, err) {
		return
	}
	options, result, err := service.options(ctx, token)
	if !handleNotificationOptionsResult(response, options, result, err) {
		return
	}
	detail, ok := safeNotificationDetail(raw, options)
	if !ok {
		writeAPIError(response, http.StatusBadGateway, 502, "notification channel type is unsupported")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": detail})
}

func (service notificationService) create(response http.ResponseWriter, request *http.Request) {
	service.upsert(response, request, "")
}

func (service notificationService) update(response http.ResponseWriter, request *http.Request) {
	id, ok := validServiceID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification channel id")
		return
	}
	service.upsert(response, request, id)
}

func (service notificationService) upsert(response http.ResponseWriter, request *http.Request, id string) {
	if !service.requireStore(response) {
		return
	}
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	var input notificationConfigRequest
	if !decodeServiceRequest(response, request, &input, "invalid notification channel request") {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Type = strings.ToLower(strings.TrimSpace(input.Type))
	if input.Name == "" || len(input.Name) > 80 || strings.ContainsAny(input.Name, "\r\n\t") {
		writeAPIError(response, http.StatusBadRequest, 400, "notification channel name is invalid")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	options, result, err := service.options(ctx, token)
	if !handleNotificationOptionsResult(response, options, result, err) {
		return
	}
	schema, found := notificationSchema(options, input.Type)
	if !found {
		writeAPIError(response, http.StatusBadRequest, 400, "unsupported notification channel type")
		return
	}
	current := map[string]any{}
	if id != "" {
		current, result, err = service.detail(ctx, token, id)
		if !handleNotificationDetailResult(response, current, result, err) {
			return
		}
		if strings.ToLower(text(current["type"])) != input.Type {
			writeAPIError(response, http.StatusBadRequest, 400, "notification channel type cannot be changed")
			return
		}
	}
	if input.Interactive && !schema.CanInteract {
		writeAPIError(response, http.StatusBadRequest, 400, "notification channel does not support interaction")
		return
	}
	events, message := validateNotificationEvents(input.Events, options.Events)
	if message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}
	config, message := mergeNotificationConfig(input.Config, input.ClearConfig, objectValue(current["config"]), schema, id == "")
	if message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}
	configJSON, _ := json.Marshal(config)
	eventsJSON, err := json.Marshal(events)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "notification events are invalid")
		return
	}
	channel := notificationconfig.Channel{
		Name: input.Name, Type: input.Type, Config: string(configJSON), Switches: string(eventsJSON),
		Interactive: notificationFlag(input.Interactive), Enabled: notificationFlag(input.Enabled),
	}
	if id != "" {
		channel.ID, _ = strconv.ParseInt(id, 10, 64)
	}
	if _, err := service.store.Save(ctx, channel); err != nil {
		if errors.Is(err, notificationconfig.ErrNotFound) {
			writeAPIError(response, http.StatusNotFound, 404, "notification channel not found")
		} else {
			writeAPIError(response, http.StatusBadGateway, 502, "notification channel could not be saved")
		}
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "通知渠道已保存"})
}

func (service notificationService) test(response http.ResponseWriter, request *http.Request) {
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id, ok := validServiceID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification channel id")
		return
	}
	if service.store == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native notification storage is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	detail, result, err := service.detail(ctx, token, id)
	if !handleNotificationDetailResult(response, detail, result, err) {
		return
	}
	if !nativeNotificationDeliveryEligible(text(detail["type"]), objectValue(detail["config"])) {
		writeAPIError(response, http.StatusNotImplemented, 501, "notification channel test is not migrated")
		return
	}
	started := time.Now()
	sent, _ := service.sendNativeNotification(ctx, text(detail["type"]), objectValue(detail["config"]), "测试", "这是一条测试消息", "", "https://github.com/0xforee/nas-tools")
	message := "测试消息发送失败"
	if sent {
		message = "测试消息已发送"
	}
	data := notificationTestData{ID: id, OK: sent, Duration: time.Since(started).Milliseconds(), Message: message}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func (service notificationService) updateStatus(response http.ResponseWriter, request *http.Request) {
	if !service.requireStore(response) {
		return
	}
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id, ok := validServiceID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification channel id")
		return
	}
	var input notificationStatusRequest
	if !decodeServiceRequest(response, request, &input, "invalid notification status request") {
		return
	}
	if (input.Enabled == nil) == (input.Interactive == nil) {
		writeAPIError(response, http.StatusBadRequest, 400, "exactly one status field is required")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	detail, result, err := service.detail(ctx, token, id)
	if !handleNotificationDetailResult(response, detail, result, err) {
		return
	}
	checked := input.Enabled
	if input.Interactive != nil {
		checked = input.Interactive
		if !interactiveNotificationTypes[strings.ToLower(text(detail["type"]))] {
			writeAPIError(response, http.StatusBadRequest, 400, "notification channel does not support interaction")
			return
		}
	}
	numericID, _ := strconv.ParseInt(id, 10, 64)
	if err := service.store.SetStatus(ctx, numericID, input.Interactive != nil, *checked); err != nil {
		if errors.Is(err, notificationconfig.ErrNotFound) {
			writeAPIError(response, http.StatusNotFound, 404, "notification channel not found")
		} else {
			writeAPIError(response, http.StatusBadGateway, 502, "notification status could not be updated")
		}
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "通知状态已更新"})
}

func (service notificationService) sendCustomMessage(response http.ResponseWriter, request *http.Request) {
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	var input customMessageRequest
	if !decodeServiceRequest(response, request, &input, "invalid custom message request") {
		return
	}
	input.Title = strings.TrimSpace(input.Title)
	input.Image = strings.TrimSpace(input.Image)
	if input.Title == "" || len(input.Title) > 200 || strings.ContainsAny(input.Title, "\r\n\x00") {
		writeAPIError(response, http.StatusBadRequest, 400, "custom message title is invalid")
		return
	}
	if len(input.Text) > 10000 || strings.ContainsRune(input.Text, '\x00') {
		writeAPIError(response, http.StatusBadRequest, 400, "custom message text is invalid")
		return
	}
	if input.Image != "" && (len(input.Image) > 2048 || !validHTTPURL(input.Image)) {
		writeAPIError(response, http.StatusBadRequest, 400, "custom message image URL is invalid")
		return
	}
	ids, message := validateCustomMessageChannelIDs(input.ChannelIDs)
	if message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	service.deliverCustomMessage(response, ctx, token, input, ids, false)
}

func (service notificationService) deliverCustomMessage(response http.ResponseWriter, ctx context.Context, token string, input customMessageRequest, ids []string, legacy bool) {
	if service.store == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native notification storage is unavailable")
		return
	}
	result, err := service.channelRecords(ctx, token)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "notification channels are unavailable")
		return
	}
	if number(result["code"]) == 403 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	if number(result["code"]) != 0 {
		writeAPIError(response, http.StatusBadGateway, 502, "notification channels are unavailable")
		return
	}
	available := map[string]bool{}
	for _, channel := range normalizeNotificationChannels(legacyPayload(result)["detail"]) {
		if channel.Enabled && channel.Configured {
			available[channel.ID] = true
		}
	}
	for _, id := range ids {
		if !available[id] {
			writeAPIError(response, http.StatusBadRequest, 400, "notification channel is unavailable")
			return
		}
	}
	records := objectValue(legacyPayload(result)["detail"])
	for _, id := range ids {
		record := objectValue(records[id])
		if !nativeNotificationDeliveryEligible(text(record["type"]), objectValue(record["config"])) {
			writeAPIError(response, http.StatusNotImplemented, 501, "notification channel delivery is not migrated")
			return
		}
	}
	for _, id := range ids {
		record := objectValue(records[id])
		sent, err := service.sendNativeNotification(ctx, text(record["type"]), objectValue(record["config"]), input.Title, input.Text, input.Image, "")
		if err != nil || !sent {
			writeAPIError(response, http.StatusBadGateway, 502, "custom message could not be sent")
			return
		}
	}
	if legacy {
		writeJSON(response, http.StatusOK, map[string]any{"code": 0})
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"code": 0, "success": true, "message": "自定义消息已发送", "data": customMessageData{ChannelCount: len(ids)},
	})
}

func (service notificationService) delete(response http.ResponseWriter, request *http.Request) {
	if !service.requireStore(response) {
		return
	}
	_, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id, ok := validServiceID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid notification channel id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	numericID, _ := strconv.ParseInt(id, 10, 64)
	if err := service.store.Delete(ctx, numericID); err != nil {
		if errors.Is(err, notificationconfig.ErrNotFound) {
			writeAPIError(response, http.StatusNotFound, 404, "notification channel not found")
		} else {
			writeAPIError(response, http.StatusBadGateway, 502, "notification channel could not be deleted")
		}
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "通知渠道已删除"})
}

func (service notificationService) requireStore(response http.ResponseWriter) bool {
	if service.store != nil {
		return true
	}
	writeAPIError(response, http.StatusServiceUnavailable, 503, "native notification storage is unavailable")
	return false
}

func (service notificationService) detail(ctx context.Context, token, id string) (map[string]any, map[string]any, error) {
	if service.store == nil {
		return nil, nil, errors.New("native notification storage is unavailable")
	}
	numericID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || numericID < 1 {
		return nil, map[string]any{"code": 404}, nil
	}
	item, err := service.store.Get(ctx, numericID)
	if errors.Is(err, notificationconfig.ErrNotFound) {
		return nil, map[string]any{"code": 404}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	record, err := nativeNotificationRecord(item)
	return record, nil, err
}

func (service notificationService) channelRecords(ctx context.Context, token string) (map[string]any, error) {
	if service.store == nil {
		return nil, errors.New("native notification storage is unavailable")
	}
	items, err := service.store.List(ctx)
	if err != nil {
		return nil, err
	}
	values := make(map[string]any, len(items))
	for _, item := range items {
		record, err := nativeNotificationRecord(item)
		if err != nil {
			return nil, err
		}
		values[strconv.FormatInt(item.ID, 10)] = record
	}
	return map[string]any{"code": 0, "data": map[string]any{"detail": values}}, nil
}

func nativeNotificationRecord(item notificationconfig.Channel) (map[string]any, error) {
	configuration := map[string]any{}
	if item.Config != "" && item.Config != "null" {
		if err := json.Unmarshal([]byte(item.Config), &configuration); err != nil {
			return nil, errors.New("invalid stored notification configuration")
		}
	}
	// The old Message cache always adds this flag to the returned config.
	configuration["interactive"] = item.Interactive
	switches := []any{}
	if item.Switches != "" && item.Switches != "null" {
		if err := json.Unmarshal([]byte(item.Switches), &switches); err != nil {
			return nil, errors.New("invalid stored notification events")
		}
	}
	return map[string]any{
		"id": item.ID, "name": item.Name, "type": item.Type, "config": configuration,
		"switchs": switches, "interactive": item.Interactive, "enabled": item.Enabled,
	}, nil
}

func notificationFlag(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (service notificationService) options(ctx context.Context, token string) (notificationOptionsData, map[string]any, error) {
	var data notificationOptionsData
	if err := json.Unmarshal(nativeNotificationOptionsJSON, &data); err != nil {
		return notificationOptionsData{}, nil, err
	}
	return data, nil, nil
}

func notificationSchema(options notificationOptionsData, ctype string) (notificationChannelOption, bool) {
	for _, channel := range options.Channels {
		if channel.Type == ctype {
			return channel, true
		}
	}
	return notificationChannelOption{}, false
}

func safeNotificationDetail(raw map[string]any, options notificationOptionsData) (notificationDetail, bool) {
	ctype := strings.ToLower(text(raw["type"]))
	schema, ok := notificationSchema(options, ctype)
	if !ok {
		return notificationDetail{}, false
	}
	config := objectValue(raw["config"])
	safe := map[string]any{}
	configured := make([]string, 0)
	for _, field := range schema.Fields {
		value, exists := config[field.Key]
		if !exists {
			continue
		}
		configured = append(configured, field.Key)
		if !field.WriteOnly {
			safe[field.Key] = value
		}
	}
	return notificationDetail{
		ID: text(raw["id"]), Name: text(raw["name"]), Type: ctype, Enabled: truthy(raw["enabled"]),
		Interactive: truthy(raw["interactive"]), CanInteract: schema.CanInteract, Events: stringList(raw["switchs"]),
		Config: safe, ConfiguredFields: configured,
	}, true
}

func validateNotificationEvents(values []string, options []notificationEvent) ([]string, string) {
	allowed := make(map[string]bool, len(options))
	for _, event := range options {
		allowed[event.ID] = true
	}
	result, seen := make([]string, 0, len(values)), map[string]bool{}
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if !allowed[value] {
			return nil, "notification event is invalid"
		}
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result, ""
}

func validateCustomMessageChannelIDs(values []string) ([]string, string) {
	if len(values) == 0 || len(values) > 50 {
		return nil, "at least one notification channel is required"
	}
	result, seen := make([]string, 0, len(values)), map[string]bool{}
	for _, raw := range values {
		id, ok := validServiceID(raw)
		if !ok {
			return nil, "notification channel id is invalid"
		}
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result, ""
}

func mergeNotificationConfig(input map[string]any, clear []string, current map[string]any, schema notificationChannelOption, create bool) (map[string]any, string) {
	fields := make(map[string]notificationFieldOption, len(schema.Fields))
	result := make(map[string]any, len(schema.Fields))
	for _, field := range schema.Fields {
		fields[field.Key] = field
		if value, ok := current[field.Key]; ok {
			result[field.Key] = value
		} else if create && field.Default != nil {
			result[field.Key] = field.Default
		}
	}
	cleared := map[string]bool{}
	for _, key := range clear {
		if _, ok := fields[key]; !ok {
			return nil, "notification configuration field is invalid"
		}
		cleared[key] = true
		delete(result, key)
	}
	for key, value := range input {
		field, ok := fields[key]
		if !ok || cleared[key] {
			return nil, "notification configuration field is invalid"
		}
		switch field.Type {
		case "switch":
			checked, ok := value.(bool)
			if !ok {
				return nil, "notification switch value is invalid"
			}
			if checked {
				result[key] = 1
			} else {
				result[key] = 0
			}
		default:
			textValue, ok := value.(string)
			if !ok {
				return nil, "notification text value is invalid"
			}
			if field.Type != "textarea" {
				textValue = strings.TrimSpace(textValue)
			}
			limit := 4096
			if field.Type == "textarea" {
				limit = 65535
			}
			if len(textValue) > limit || strings.ContainsRune(textValue, '\x00') {
				return nil, "notification configuration value is invalid"
			}
			if field.Type == "select" && !validNotificationChoice(textValue, field.Options) {
				return nil, "notification selection is invalid"
			}
			if textValue == "" {
				delete(result, key)
			} else {
				result[key] = textValue
			}
		}
	}
	for _, field := range schema.Fields {
		if field.Required && strings.TrimSpace(text(result[field.Key])) == "" {
			return nil, field.Title + " is required"
		}
	}
	return result, ""
}

func validNotificationChoice(value string, choices []notificationFieldChoice) bool {
	for _, choice := range choices {
		if choice.Value == value {
			return true
		}
	}
	return false
}

func normalizeNotificationChannels(value any) []notificationChannel {
	labels := make(map[string]string, len(notificationEvents))
	for _, event := range notificationEvents {
		labels[event.ID] = event.Label
	}
	items := make([]notificationChannel, 0)
	for _, entry := range entries(value) {
		item := objectValue(entry.value)
		if len(item) == 0 {
			continue
		}
		id := text(item["id"])
		if id == "" {
			id = entry.key
		}
		ctype := strings.ToLower(strings.TrimSpace(text(item["type"])))
		name := strings.TrimSpace(text(item["name"]))
		typeLabel := notificationTypeLabels[ctype]
		if typeLabel == "" {
			typeLabel = ctype
		}
		if name == "" {
			name = typeLabel
		}
		switches := stringList(item["switchs"])
		switchLabels := make([]string, 0, len(switches))
		for _, event := range switches {
			if label := labels[event]; label != "" {
				switchLabels = append(switchLabels, label)
			}
		}
		items = append(items, notificationChannel{
			ID: id, Name: name, Type: ctype, TypeLabel: typeLabel, Enabled: truthy(item["enabled"]),
			Interactive: truthy(item["interactive"]), CanInteract: interactiveNotificationTypes[ctype],
			Configured: notificationConfigPresent(item["config"]), Switches: switches, SwitchLabels: switchLabels,
		})
	}
	sort.SliceStable(items, func(left, right int) bool {
		leftID, leftOK := parsePositiveID(items[left].ID)
		rightID, rightOK := parsePositiveID(items[right].ID)
		if leftOK && rightOK {
			return leftID < rightID
		}
		return items[left].Name < items[right].Name
	})
	return items
}

func notificationConfigPresent(value any) bool {
	for key, item := range objectValue(value) {
		if key != "interactive" && strings.TrimSpace(text(item)) != "" {
			return true
		}
	}
	return false
}

func stringList(value any) []string {
	items := sliceOrJSON(value)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if value := strings.TrimSpace(text(item)); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func parsePositiveID(value string) (int, bool) {
	parsed := int(number(value))
	return parsed, parsed > 0
}

func handleNotificationDetailResult(response http.ResponseWriter, detail, result map[string]any, err error) bool {
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "notification channel is unavailable")
		return false
	}
	if number(result["code"]) == 403 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return false
	}
	if len(detail) == 0 {
		writeAPIError(response, http.StatusNotFound, 404, "notification channel not found")
		return false
	}
	return true
}

func handleNotificationOptionsResult(response http.ResponseWriter, data notificationOptionsData, result map[string]any, err error) bool {
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "notification options are unavailable")
		return false
	}
	if number(result["code"]) == 403 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return false
	}
	if len(data.Channels) == 0 {
		writeAPIError(response, http.StatusBadGateway, 502, "notification options are unavailable")
		return false
	}
	return true
}
