package desk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T, binary string) *App {
	t.Helper()
	dir := t.TempDir()
	a, err := New(filepath.Join(dir, "models"), binary, filepath.Join(dir, "data"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}
func request(t *testing.T, a *App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

func TestModelInventoryAndConfinement(t *testing.T) {
	a := testApp(t, "missing-llama-server")
	if err := os.WriteFile(filepath.Join(a.models, "model.gguf"), []byte("GGUFtest"), 0640); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.gguf")
	_ = os.WriteFile(outside, []byte("GGUFsecret"), 0600)
	if err := os.Symlink(outside, filepath.Join(a.models, "escape.gguf")); err != nil {
		t.Fatal(err)
	}
	models, err := a.inventory()
	if err != nil || len(models) != 1 {
		t.Fatalf("inventory: %+v %v", models, err)
	}
	for _, path := range []string{"../secret.gguf", "escape.gguf", "/etc/passwd"} {
		if _, err := a.modelPath(path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	if rec := request(t, a, "GET", "/api/state", ""); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
}

func TestRejectCrossSiteMutations(t *testing.T) {
	a := testApp(t, "unused")
	r := httptest.NewRequest("POST", "http://localhost/api/downloads", strings.NewReader(`{"urls":[]}`))
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestDownloadTargets(t *testing.T) {
	valid := "https://huggingface.co/owner/repo/resolve/main/model.gguf?download=true"
	got, err := downloadTarget(valid)
	if err != nil || got != "owner/repo/main/model.gguf" {
		t.Fatalf("%s %v", got, err)
	}
	for _, raw := range []string{"http://huggingface.co/o/r/resolve/main/x.gguf", "https://example.com/o/r/resolve/main/x.gguf", "https://huggingface.co/o/r/resolve/main/../x.gguf", "https://huggingface.co/o/r/blob/main/x.gguf", "https://huggingface.co/o/r/resolve/main/README.md"} {
		if _, err := downloadTarget(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestRepositoryDownloadInputs(t *testing.T) {
	urls, err := repositoryURLs(" owner/repo ", "", []string{"model.gguf", "subfolder/model #1.gguf"})
	if err != nil || len(urls) != 2 || urls[0] != "https://huggingface.co/owner/repo/resolve/main/model.gguf" || urls[1] != "https://huggingface.co/owner/repo/resolve/main/subfolder/model%20%231.gguf" {
		t.Fatalf("bad repository URLs: %v %v", urls, err)
	}
	urls, err = repositoryURLs("owner/repo", "abc123", []string{"model.gguf"})
	if err != nil || !strings.Contains(urls[0], "/resolve/abc123/") {
		t.Fatalf("revision lost: %v %v", urls, err)
	}
	for _, input := range []struct {
		repo, revision string
		files          []string
	}{
		{"owner", "", []string{"model.gguf"}},
		{"https://huggingface.co/owner/repo", "", []string{"model.gguf"}},
		{"owner/../repo", "", []string{"model.gguf"}},
		{"owner/repo", "", nil},
		{"owner/repo", "", []string{"../model.gguf"}},
		{"owner/repo", "", []string{"/model.gguf"}},
		{"owner/repo", "", []string{"model*.gguf"}},
		{"owner/repo", "", []string{"README.md"}},
		{"owner/repo", "../main", []string{"model.gguf"}},
	} {
		if _, err := repositoryURLs(input.repo, input.revision, input.files); err == nil {
			t.Fatalf("accepted invalid input: %+v", input)
		}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDownloadAPIWithRepositoryAndFiles(t *testing.T) {
	a := testApp(t, "unused")
	a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "huggingface.co" || !strings.HasPrefix(r.URL.Path, "/owner/repo/resolve/main/") {
			t.Errorf("wrong download URL: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("GGUFtest")), ContentLength: 8}, nil
	})}
	rec := request(t, a, "POST", "/api/downloads", `{"repo":"owner/repo","files":["first.gguf","nested/second.gguf"]}`)
	if rec.Code != 202 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var jobs []Job
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil || len(jobs) != 2 {
		t.Fatalf("bad jobs: %s %v", rec.Body.String(), err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for _, job := range jobs {
		for {
			a.mu.Lock()
			status, detail := a.jobs[job.ID].Status, a.jobs[job.ID].Error
			a.mu.Unlock()
			if status == "completed" {
				break
			}
			if status == "failed" || time.Now().After(deadline) {
				t.Fatalf("download %s: %s %s", job.File, status, detail)
			}
			time.Sleep(10 * time.Millisecond)
		}
		data, err := os.ReadFile(filepath.Join(a.models, filepath.FromSlash(job.File)))
		if err != nil || string(data) != "GGUFtest" {
			t.Fatalf("downloaded %q: %q %v", job.File, data, err)
		}
	}
	rec = request(t, a, "POST", "/api/downloads", `{"repo":"owner/repo","files":["other.gguf"],"urls":["https://huggingface.co/owner/repo/resolve/main/other.gguf"]}`)
	if rec.Code != 400 {
		t.Fatal("accepted mixed URL and repository input")
	}
}

func TestDownloadResumePublishesOnlyCompleteGGUF(t *testing.T) {
	a := testApp(t, "unused")
	content := []byte("GGUFmodel payload")
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=6-" || r.Header.Get("If-Range") != `"version1"` {
			t.Errorf("bad resume headers: %v", r.Header)
		}
		w.Header().Set("ETag", `"version1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 6-%d/%d", len(content)-1, len(content)))
		w.WriteHeader(206)
		_, _ = w.Write(content[6:])
	}))
	defer source.Close()
	j := &Job{ID: "test", URL: source.URL, File: "model.gguf", ETag: `"version1"`}
	a.jobs[j.ID] = j
	if err := os.WriteFile(filepath.Join(a.models, "model.gguf.part"), content[:6], 0640); err != nil {
		t.Fatal(err)
	}
	if err := a.transfer(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(a.models, "model.gguf"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(a.models, "model.gguf.part")); !os.IsNotExist(err) {
		t.Fatal("partial file remained")
	}
	if err := a.transfer(context.Background(), j); err == nil {
		t.Fatal("overwrote an existing model")
	}
}

func TestChangedSourceRestartsPartialAndRejectsHTML(t *testing.T) {
	for _, content := range []string{"GGUFnew version", "<html>not a model</html>"} {
		t.Run(content[:4], func(t *testing.T) {
			a := testApp(t, "unused")
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"v2"`)
				_, _ = io.WriteString(w, content)
			}))
			defer source.Close()
			j := &Job{ID: "test", URL: source.URL, File: "model.gguf", ETag: `"v1"`}
			a.jobs[j.ID] = j
			_ = os.WriteFile(filepath.Join(a.models, "model.gguf.part"), []byte("old partial"), 0640)
			err := a.transfer(context.Background(), j)
			if strings.HasPrefix(content, "GGUF") {
				got, _ := os.ReadFile(filepath.Join(a.models, "model.gguf"))
				if err != nil || string(got) != content {
					t.Fatalf("%q %v", got, err)
				}
			} else {
				if err == nil {
					t.Fatal("accepted HTML")
				}
				if _, err = os.Stat(filepath.Join(a.models, "model.gguf")); !os.IsNotExist(err) {
					t.Fatal("published invalid model")
				}
			}
		})
	}
}

func TestProcessLifecycleAndSavedProfile(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "fake-llama-server")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ntrap 'echo stopped; exit 0' TERM\necho ready:$TEST_VALUE\nwhile :; do sleep 1; done\n"), 0755); err != nil {
		t.Fatal(err)
	}
	a := testApp(t, binary)
	_ = os.WriteFile(filepath.Join(a.models, "test.gguf"), []byte("GGUFtest"), 0640)
	body := `{"model":"test.gguf","context":8192,"gpu_layers":99,"port":18080,"args":["--jinja"],"env":{"TEST_VALUE":"value with spaces"}}`
	rec := request(t, a, "POST", "/api/runs", body)
	if rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	var run Run
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.Launch.Name != "test" || !strings.Contains(strings.Join(run.Command, " "), "--alias test") {
		t.Fatalf("model name was not passed to native server: %+v", run)
	}
	duplicate := strings.Replace(body, `"port":18080`, `"port":18081`, 1)
	if rec := request(t, a, "POST", "/api/runs", duplicate); rec.Code != 400 {
		t.Fatalf("duplicate model name accepted: %s", rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec = request(t, a, "GET", "/api/runs/"+run.ID+"/logs", "")
		if strings.Contains(rec.Body.String(), "ready:value with spaces") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no child output")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec = request(t, a, "DELETE", "/api/models?path=test.gguf", ""); rec.Code != 400 {
		t.Fatal("deleted a running model")
	}
	if rec = request(t, a, "POST", "/api/runs/"+run.ID+"/stop", `{}`); rec.Code != 202 {
		t.Fatal(rec.Body.String())
	}
	for {
		a.mu.Lock()
		status := a.runs[run.ID].Status
		a.mu.Unlock()
		if status == "stopped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	restored, err := New(a.models, binary, a.data, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.profiles["test.gguf"].Env["TEST_VALUE"] != "value with spaces" {
		t.Fatal("lost saved launch profile")
	}
}
