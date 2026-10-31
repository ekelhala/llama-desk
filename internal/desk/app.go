package desk

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var assets embed.FS

type Model struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}
type App struct {
	mu                   sync.Mutex
	models, binary, data string
	jobs                 map[string]*Job
	runs                 map[string]*Run
	profiles             map[string]Launch
	slots                chan struct{}
	ctx                  context.Context
	cancel               context.CancelFunc
	workers              sync.WaitGroup
	closeOnce            sync.Once
	client               *http.Client
}
type savedState struct {
	Jobs     map[string]*Job   `json:"jobs"`
	Profiles map[string]Launch `json:"profiles"`
}

func New(models, binary, data string, parallel int) (*App, error) {
	if parallel < 1 || parallel > 32 {
		return nil, errors.New("downloads must be between 1 and 32")
	}
	var err error
	models, err = filepath.Abs(models)
	if err != nil {
		return nil, err
	}
	data, err = filepath.Abs(data)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{models, data} {
		if err = os.MkdirAll(dir, 0750); err != nil {
			return nil, err
		}
	}
	models, err = filepath.EvalSymlinks(models)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{models: models, binary: binary, data: data, jobs: map[string]*Job{}, runs: map[string]*Run{}, profiles: map[string]Launch{}, slots: make(chan struct{}, parallel), ctx: ctx, cancel: cancel, client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 30 * time.Second}}}
	contents, err := os.ReadFile(filepath.Join(data, "state.json"))
	if err == nil {
		var state savedState
		if err = json.Unmarshal(contents, &state); err != nil {
			cancel()
			return nil, fmt.Errorf("read state: %w", err)
		}
		if state.Jobs != nil {
			a.jobs = state.Jobs
		}
		if state.Profiles != nil {
			a.profiles = state.Profiles
		}
		for _, job := range a.jobs {
			if job.Status == "downloading" || job.Status == "queued" {
				job.Status = "interrupted"
				job.Error = "Service stopped; click Resume to continue."
			}
		}
	} else if !os.IsNotExist(err) {
		cancel()
		return nil, err
	}
	return a, nil
}

func (a *App) Close() {
	a.closeOnce.Do(func() {
		a.cancel()
		a.mu.Lock()
		for _, r := range a.runs {
			if r.stop != nil {
				r.stop()
			}
		}
		a.mu.Unlock()
		a.workers.Wait()
	})
}

// Caller holds a.mu. State contains user-supplied environment values: keep it private.
func (a *App) save() error {
	b, err := json.MarshalIndent(savedState{a.jobs, a.profiles}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.data, "state.json")
	if err = os.WriteFile(path+".tmp", b, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func id() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, err error) { respond(w, 400, map[string]string{"error": err.Error()}) }
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("POST /api/downloads", a.addDownload)
	mux.HandleFunc("POST /api/downloads/{id}/{action}", a.jobAction)
	mux.HandleFunc("POST /api/runs", a.launch)
	mux.HandleFunc("POST /api/runs/{id}/stop", a.stopRun)
	mux.HandleFunc("GET /api/runs/{id}/logs", a.logs)
	mux.HandleFunc("GET /api/runs/{id}/health", a.health)
	mux.HandleFunc("DELETE /api/models", a.deleteModel)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}) })
	web, _ := fs.Sub(assets, "web")
	mux.Handle("/", http.FileServer(http.FS(web)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'self'; base-uri 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "cross-site request denied", 403)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request denied", 403)
					return
				}
			}
			if r.Method == "POST" && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "application/json required", 415)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *App) inventory() ([]Model, error) {
	models := []Model{}
	err := filepath.WalkDir(a.models, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(strings.ToLower(d.Name()), ".gguf") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(a.models, path)
		models = append(models, Model{filepath.ToSlash(rel), info.Size(), info.ModTime()})
		return nil
	})
	sort.Slice(models, func(i, j int) bool { return models[i].Path < models[j].Path })
	return models, err
}

func (a *App) modelPath(rel string) (string, error) {
	if !filepath.IsLocal(rel) || !strings.HasSuffix(strings.ToLower(rel), ".gguf") {
		return "", errors.New("select a GGUF inside the model directory")
	}
	path := filepath.Join(a.models, rel)
	actual, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if actual != path {
		return "", errors.New("symlinked model files are not supported")
	}
	info, err := os.Stat(actual)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("model must be a regular file")
	}
	return path, nil
}

func (a *App) state(w http.ResponseWriter, r *http.Request) {
	models, err := a.inventory()
	if err != nil {
		fail(w, err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	jobs := []*Job{}
	for _, j := range a.jobs {
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Created.After(jobs[j].Created) })
	runs := []*Run{}
	for _, run := range a.runs {
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Started.After(runs[j].Started) })
	respond(w, 200, map[string]any{"settings": map[string]any{"models": a.models, "binary": a.binary, "parallel_downloads": cap(a.slots)}, "models": models, "jobs": jobs, "runs": runs, "profiles": a.profiles})
}

func (a *App) deleteModel(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rel := r.URL.Query().Get("path")
	path, err := a.modelPath(rel)
	if err != nil {
		fail(w, err)
		return
	}
	for _, run := range a.runs {
		if run.Status == "running" || run.Status == "stopping" {
			if run.Launch.Model == rel {
				fail(w, errors.New("stop the model server before deleting its file"))
				return
			}
		}
	}
	if err = os.Remove(path); err != nil {
		fail(w, err)
		return
	}
	respond(w, 200, map[string]bool{"deleted": true})
}

func getJSON(client *http.Client, ctx context.Context, url string) (int, json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	if !json.Valid(b) {
		return 0, nil, errors.New("server returned a non-JSON response")
	}
	return res.StatusCode, b, nil
}
