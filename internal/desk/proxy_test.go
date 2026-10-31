package desk

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func addBackend(t *testing.T, a *App, name string, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	number, _ := strconv.Atoi(port)
	a.runs[name] = &Run{ID: name, Launch: Launch{Name: name, Port: number}, Status: "running", Started: time.Now(), ctx: context.Background()}
	return server
}

func TestProxyRoutesByNameWithoutChangingPayload(t *testing.T) {
	a := testApp(t, "unused")
	addBackend(t, a, "alpha", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.URL.RawQuery != "test=1" {
			t.Errorf("wrong target: %s", r.URL)
		}
		for _, header := range []string{"Authorization", "X-Api-Key", "X-Litellm-Api-Key", "Cookie"} {
			if r.Header.Get(header) != "" {
				t.Errorf("leaked %s", header)
			}
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"model":"alpha","messages":[],"temperature":0.7,"extra":{"custom":true}}` {
			t.Errorf("changed payload: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Backend", "alpha")
		_, _ = io.WriteString(w, `{"model":"alpha","choices":[]}`)
	})
	addBackend(t, a, "beta", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":{"message":"loading beta"}}`)
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions?test=1", strings.NewReader(`{"model":"alpha","messages":[],"temperature":0.7,"extra":{"custom":true}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer private-key")
	req.Header.Set("X-Api-Key", "private-key")
	req.Header.Set("X-Litellm-Api-Key", "private-key")
	req.Header.Set("Cookie", "session=secret")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("X-Backend") != "alpha" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = request(t, a, "POST", "/v1/chat/completions", `{"model":"beta"}`)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "loading beta") {
		t.Fatalf("upstream error lost: %d %s", rec.Code, rec.Body.String())
	}
	rec = request(t, a, "GET", "/v1/models", "")
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data) != 2 || list.Data[0].ID != "alpha" || list.Data[1].ID != "beta" {
		t.Fatalf("bad model list: %s", rec.Body.String())
	}
	a.mu.Lock()
	a.runs["alpha"].Status = "stopped"
	a.mu.Unlock()
	rec = request(t, a, "POST", "/v1/completions", `{"model":"alpha"}`)
	if rec.Code != 404 {
		t.Fatalf("routed to stopped process: %d", rec.Code)
	}
}

func TestProxyFlushesStreamingTokensBeforeCompletion(t *testing.T) {
	a := testApp(t, "unused")
	release := make(chan struct{})
	defer close(release)
	addBackend(t, a, "stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	desk := httptest.NewServer(a.Handler())
	defer desk.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Post(desk.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"stream","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: {\"choices\":[]}\n" {
		t.Fatalf("first event not flushed: %q %v", line, err)
	}
	// Receiving the first event while the backend is blocked demonstrates no buffering.
}

func TestProxyRejectsUnknownAndMalformedRequests(t *testing.T) {
	a := testApp(t, "unused")
	for _, body := range []string{`{`, `{"messages":[]}`, `{"model":42}`} {
		rec := request(t, a, "POST", "/v1/embeddings", body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_request_error") {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
	rec := request(t, a, "POST", "/v1/responses", `{"model":"missing"}`)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// An exited process must fail clearly, rather than falling back to another model.
	addBackend(t, a, "offline", func(w http.ResponseWriter, r *http.Request) {}).Close()
	rec = request(t, a, "POST", "/v1/completions", `{"model":"offline"}`)
	if rec.Code != 502 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestProxyCancelsUpstreamWhenClientDisconnects(t *testing.T) {
	a := testApp(t, "unused")
	cancelled := make(chan struct{})
	addBackend(t, a, "cancel", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	})
	desk := httptest.NewServer(a.Handler())
	defer desk.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", desk.URL+"/v1/chat/completions", strings.NewReader(`{"model":"cancel","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err = bufio.NewReader(response.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream kept generating after client disconnected")
	}
}
