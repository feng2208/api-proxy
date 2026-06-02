package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ModelData represents a single model in the /v1/models response.
type ModelData struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelsResponse represents the /v1/models response format.
type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelData `json:"data"`
}

// ProviderHandler holds the runtime state for a single provider of a model.
type ProviderHandler struct {
	config     ProviderConfig
	keyManager *KeyManager
	client     *http.Client
}

func NewProviderHandler(p ProviderConfig, km *KeyManager) *ProviderHandler {
	transport := buildTransport(p.Proxy.URL)
	client := &http.Client{
		Transport: transport,
		Timeout:   0, // Timeout handled via context
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &ProviderHandler{
		config:     p,
		keyManager: km,
		client:     client,
	}
}

// ModelRouter handles routing to providers for a specific model.
type ModelRouter struct {
	modelID  string
	handlers []*ProviderHandler
	next     uint32
}

func (mr *ModelRouter) getNextHandler() *ProviderHandler {
	n := atomic.AddUint32(&mr.next, 1)
	return mr.handlers[(int(n)-1)%len(mr.handlers)]
}

// ProxyRouter is the main router for the proxy.
type ProxyRouter struct {
	basePath string
	models   map[string]*ModelRouter
	apiKeys  map[string]bool
	limiter  *ClientRateLimiter
	cfg      *Config
}

// NewProxyRouter creates a ProxyRouter from the config.
func NewProxyRouter(cfg *Config, keyManagers map[string]*KeyManager) *ProxyRouter {
	models := make(map[string]*ModelRouter)
	for _, m := range cfg.Models {
		mr := &ModelRouter{
			modelID:  m.Name,
			handlers: make([]*ProviderHandler, len(m.Providers)),
		}
		for i, p := range m.Providers {
			km := keyManagers[p.Name]
			mr.handlers[i] = NewProviderHandler(p, km)
		}
		models[m.Name] = mr
	}

	apiKeys := make(map[string]bool, len(cfg.APIKeys))
	for _, k := range cfg.APIKeys {
		apiKeys[k] = true
	}

	return &ProxyRouter{
		basePath: cfg.BasePath,
		models:   models,
		apiKeys:  apiKeys,
		limiter:  NewClientRateLimiter(cfg.ClientRateLimit),
		cfg:      cfg,
	}
}

func writeErrorJSON(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	
	// OpenAI compatible error format
	errResp := map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    nil,
		},
	}
	
	json.NewEncoder(w).Encode(errResp)
}

func extractBearerToken(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		return strings.TrimSpace(authHeader[7:])
	}
	return authHeader
}

func (pr *ProxyRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
		return
	}

	// All OpenAI compatible requests should be under basePath
	if !strings.HasPrefix(r.URL.Path, pr.basePath) {
		writeErrorJSON(w, http.StatusNotFound, "Not Found")
		return
	}

	// Authenticate the client
	tokenValue := extractBearerToken(r)

	if tokenValue == "" || !pr.apiKeys[tokenValue] {
		writeErrorJSON(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	if !pr.limiter.Allow(tokenValue) {
		writeErrorJSON(w, http.StatusTooManyRequests, "Too Many Requests")
		return
	}

	trimmedPath := strings.TrimPrefix(r.URL.Path, pr.basePath)

	// Handle /v1/models
	if trimmedPath == "/models" && r.Method == "GET" {
		pr.handleModels(w)
		return
	}

	// Other paths require reading body to extract model
	var bodyBytes []byte
	var err error
	if r.Body != nil {
		bodyBytes, err = io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "failed to read request body")
			return
		}
	}

	var reqBody map[string]interface{}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &reqBody); err != nil {
			writeErrorJSON(w, http.StatusBadRequest, "invalid json body")
			return
		}
	}

	modelValue, ok := reqBody["model"].(string)
	if !ok || modelValue == "" {
		writeErrorJSON(w, http.StatusBadRequest, "missing or invalid model field in request")
		return
	}

	modelRouter, exists := pr.models[modelValue]
	if !exists {
		writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("model %q not configured", modelValue))
		return
	}

	handler := modelRouter.getNextHandler()

	// Rewrite the model field
	reqBody["model"] = handler.config.Model
	newBodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to rewrite request body")
		return
	}

	// Build upstream URL
	upstreamPath := trimmedPath
	upstreamURL := strings.TrimRight(handler.config.Upstream, "/") + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	handler.forwardRequest(w, r, upstreamURL, modelRouter.modelID, newBodyBytes)
}

func (ph *ProviderHandler) forwardRequest(w http.ResponseWriter, r *http.Request, upstreamURL string, modelID string, bodyBytes []byte) {
	key, keyIndex, err := ph.keyManager.GetKey(modelID, ph.config.ModelRateLimit)
	if err != nil {
		log.Printf("provider=%s model=%s: all keys in cooldown or rate limited", ph.config.Name, modelID)
		writeErrorJSON(w, http.StatusServiceUnavailable, "Service Unavailable: all upstream keys exhausted or rate limited")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), ph.config.TimeoutDuration)
	defer cancel()

	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	upstreamReq, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bodyReader)
	if err != nil {
		log.Printf("provider=%s: failed to create upstream request: %v", ph.config.Name, err)
		writeErrorJSON(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	copyHeaders(upstreamReq.Header, r.Header)
	// OpenAI standard uses Bearer auth
	upstreamReq.Header.Set("Authorization", "Bearer "+key)
	// Update Content-Length since body might have changed size
	upstreamReq.Header.Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))
	upstreamReq.ContentLength = int64(len(bodyBytes))

	resp, err := ph.client.Do(upstreamReq)
	if err != nil {
		log.Printf("provider=%s: upstream request error: %v", ph.config.Name, err)
		writeErrorJSON(w, http.StatusBadGateway, "Bad Gateway")
		return
	}
	defer resp.Body.Close()

	// Handle key failure statuses
	switch {
	case resp.StatusCode == http.StatusTooManyRequests: // 429
		ph.keyManager.Mark429(keyIndex)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden: // 401, 403
		ph.keyManager.MarkFailed(keyIndex)
	default:
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			ph.keyManager.ResetFailures(keyIndex)
		}
	}

	streamResponse(w, resp)
}

func (pr *ProxyRouter) handleModels(w http.ResponseWriter) {
	resp := ModelsResponse{
		Object: "list",
		Data:   make([]ModelData, 0, len(pr.cfg.Models)),
	}

	now := time.Now().Unix()
	for _, m := range pr.cfg.Models {
		resp.Data = append(resp.Data, ModelData{
			ID:      m.Name,
			Object:  "model",
			Created: now,
			OwnedBy: "system",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}
