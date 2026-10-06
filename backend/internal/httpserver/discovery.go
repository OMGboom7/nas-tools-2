package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

type discoveryService struct {
	client       *http.Client
	images       *mediaImageProxy
	config       *config.Store
	databasePath string
}

type discoveryRequest struct {
	Category string `json:"category"`
	Page     int    `json:"page"`
}

type discoveryData struct {
	Category string           `json:"category"`
	Page     int              `json:"page"`
	Items    []discoveryMedia `json:"items"`
}

type discoveryMedia struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Year       string `json:"year"`
	Type       string `json:"type"`
	MediaType  string `json:"mediaType"`
	Vote       string `json:"vote"`
	Image      string `json:"image"`
	Backdrop   string `json:"backdrop"`
	Overview   string `json:"overview"`
	Link       string `json:"link"`
	Subscribed bool   `json:"subscribed"`
}

var discoveryCategories = map[string]string{
	"trending":       "/trending/all/week",
	"popular-movies": "/movie/popular",
	"popular-series": "/tv/popular",
	"new-movies":     "/movie/now_playing",
	"new-series":     "/tv/on_the_air",
}

func (service discoveryService) serveHTTP(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	var input discoveryRequest
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid discovery request")
		return
	}
	endpoint, ok := discoveryCategories[input.Category]
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "unsupported discovery category")
		return
	}
	if input.Page == 0 {
		input.Page = 1
	}
	if input.Page < 1 || input.Page > 100 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid discovery page")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	items, err := service.nativeRecommendations(ctx, endpoint, input.Page)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "media recommendations are unavailable")
		return
	}
	data := discoveryData{Category: input.Category, Page: input.Page, Items: items}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}
