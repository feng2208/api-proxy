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

// authenticate validates client credentials and client-side rate limits.
func (pr *ProxyRouter) authenticate(r *http.Request) (int, string) {
	tokenValue := extractBearerToken(r)
	if tokenValue == "" || !pr.apiKeys[tokenValue] {
		return http.StatusUnauthorized, "Unauthorized"
	}
	if !pr.limiter.Allow(tokenValue) {
		return http.StatusTooManyRequests, "Too Many Requests"
	}
	return 0, ""
}

// parseRequestBody reads and parses the HTTP request body as JSON.
func parseRequestBody(r *http.Request) ([]byte, map[string]interface{}, error) {
	var bodyBytes []byte
	var err error
	if r.Body != nil {
		bodyBytes, err = io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read request body")
		}
	}

	var reqBody map[string]interface{}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &reqBody); err != nil {
			return nil, nil, fmt.Errorf("invalid json body")
		}
	}
	return bodyBytes, reqBody, nil
}

// selectProvider chooses the next available provider handler and leases a key.
func (pr *ProxyRouter) selectProvider(modelRouter *ModelRouter, modelValue string) (*ProviderHandler, string, int, error) {
	var handler *ProviderHandler
	var key string
	var keyIndex int
	var getErr error

	n := len(modelRouter.handlers)
	startIndex := int(atomic.AddUint32(&modelRouter.next, 1)-1) % n

	for i := 0; i < n; i++ {
		h := modelRouter.handlers[(startIndex+i)%n]
		k, idx, err := h.keyManager.GetKey(modelRouter.modelID, h.config.ModelRateLimit)
		if err == nil {
			handler = h
			key = k
			keyIndex = idx
			break
		}
		getErr = err
	}

	if handler == nil {
		if getErr != nil {
			return nil, "", -1, getErr
		}
		return nil, "", -1, fmt.Errorf("all providers exhausted or rate limited")
	}

	return handler, key, keyIndex, nil
}

// rewriteBody updates the requested model with the provider-specific name.
func rewriteBody(reqBody map[string]interface{}, targetModel string) ([]byte, error) {
	reqBody["model"] = targetModel
	return json.Marshal(reqBody)
}

// buildUpstreamURL formats the final upstream API URL.
func buildUpstreamURL(upstream, path, rawQuery string) string {
	url := strings.TrimRight(upstream, "/") + path
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	return url
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

	// 1. Authenticate the client
	if errCode, errMsg := pr.authenticate(r); errMsg != "" {
		writeErrorJSON(w, errCode, errMsg)
		return
	}

	trimmedPath := strings.TrimPrefix(r.URL.Path, pr.basePath)

	// 2. Handle /v1/models metadata endpoint
	if trimmedPath == "/models" && r.Method == http.MethodGet {
		pr.handleModels(w)
		return
	}

	// 3. Parse request payload
	_, reqBody, err := parseRequestBody(r)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
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

	// 4. Select provider and lease key with automatic failover
	handler, key, keyIndex, err := pr.selectProvider(modelRouter, modelValue)
	if err != nil {
		log.Printf("model=%s: all providers exhausted or rate limited. Last error: %v", modelValue, err)
		writeErrorJSON(w, http.StatusServiceUnavailable, "Service Unavailable: all upstream keys exhausted or rate limited")
		return
	}

	// 5. Rewrite request body
	newBodyBytes, err := rewriteBody(reqBody, handler.config.Model)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to rewrite request body")
		return
	}

	// 6. Build upstream URL & forward
	upstreamURL := buildUpstreamURL(handler.config.Upstream, trimmedPath, r.URL.RawQuery)
	handler.forwardRequest(w, r, upstreamURL, key, keyIndex, newBodyBytes)
}

func (ph *ProviderHandler) forwardRequest(w http.ResponseWriter, r *http.Request, upstreamURL string, key string, keyIndex int, bodyBytes []byte) {
	log.Printf("using provider=%s key[%d] for request", ph.config.Name, keyIndex)

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
