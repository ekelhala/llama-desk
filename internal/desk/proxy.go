package desk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
)

func proxyError(w http.ResponseWriter, status int, kind, message string) {
	respond(w, status, map[string]any{"error": map[string]string{"message": message, "type": kind, "code": kind}})
}

// Listing managed processes avoids a health-check fanout on every model lookup.
// A loading child is listed too; its own HTTP 503 propagates to the caller.
func (a *App) proxyModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	models := []model{}
	a.mu.Lock()
	for _, run := range a.runs {
		if run.Status == "running" {
			models = append(models, model{run.Launch.Name, "model", run.Started.Unix(), "llama-desk"})
		}
	}
	a.mu.Unlock()
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	respond(w, 200, map[string]any{"object": "list", "data": models})
}

func (a *App) proxyInference(w http.ResponseWriter, r *http.Request) {
	// Large enough for long prompts and small inline images, but never unbounded.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		proxyError(w, 413, "invalid_request_error", "request exceeds the 32 MiB limit")
		return
	}
	var input struct {
		Model string `json:"model"`
	}
	if err = json.Unmarshal(body, &input); err != nil || input.Model == "" {
		proxyError(w, 400, "invalid_request_error", "provide a JSON object with a nonempty model name")
		return
	}
	a.mu.Lock()
	port := 0
	var runContext context.Context
	for _, run := range a.runs {
		if run.Status == "running" && run.Launch.Name == input.Model {
			port = run.Launch.Port
			runContext = run.ctx
			break
		}
	}
	a.mu.Unlock()
	if port == 0 {
		proxyError(w, 404, "model_not_found", "model is not running; start it in llama-desk first")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if runContext != nil {
		stop := context.AfterFunc(runContext, cancel)
		defer stop()
	}
	r = r.WithContext(ctx)
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	proxy := &httputil.ReverseProxy{
		Transport:     a.proxyTransport,
		FlushInterval: -1, // Flush each write, including server-sent events.
		Rewrite: func(out *httputil.ProxyRequest) {
			out.SetURL(target)
			// Gateway credentials terminate at LiteLLM. Never pass them to children.
			out.Out.Header.Del("Authorization")
			out.Out.Header.Del("X-Api-Key")
			out.Out.Header.Del("X-Litellm-Api-Key")
			out.Out.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			proxyError(w, 502, "upstream_unavailable", "the selected llama-server could not be reached")
		},
	}
	proxy.ServeHTTP(w, r)
}
