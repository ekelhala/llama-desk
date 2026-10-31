package desk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

type Launch struct {
	Name      string            `json:"name"`
	Model     string            `json:"model"`
	Context   int               `json:"context"`
	GPULayers int               `json:"gpu_layers"`
	Port      int               `json:"port"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
}
type Run struct {
	ID      string     `json:"id"`
	Launch  Launch     `json:"launch"`
	Command []string   `json:"command"`
	PID     int        `json:"pid"`
	Status  string     `json:"status"`
	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended,omitempty"`
	Error   string     `json:"error,omitempty"`
	stop    context.CancelFunc
	ctx     context.Context
	log     *logBuffer
}
type logBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > 128<<10 {
		b.data = bytes.Clone(b.data[len(b.data)-(128<<10):])
	}
	return len(p), nil
}
func (b *logBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }

func (a *App) launch(w http.ResponseWriter, r *http.Request) {
	var spec Launch
	if err := decode(w, r, &spec); err != nil {
		fail(w, err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	model, err := a.modelPath(spec.Model)
	if err != nil {
		fail(w, err)
		return
	}
	if err = parsePort(spec.Port); err != nil {
		fail(w, err)
		return
	}
	if spec.Context < 1 || spec.GPULayers < 0 {
		fail(w, errors.New("context must be positive and GPU layers nonnegative"))
		return
	}
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		spec.Name = strings.TrimSuffix(filepath.Base(spec.Model), filepath.Ext(spec.Model))
	}
	if spec.Name == "" || strings.Contains(spec.Name, ",") || strings.IndexFunc(spec.Name, unicode.IsControl) >= 0 || len(spec.Name) > 256 {
		fail(w, errors.New("model name must be at most 256 characters and cannot contain commas or control characters"))
		return
	}
	for _, run := range a.runs {
		if (run.Status == "running" || run.Status == "stopping") && run.Launch.Port == spec.Port {
			fail(w, errors.New("that port is already used by a managed server"))
			return
		}
		if (run.Status == "running" || run.Status == "stopping") && run.Launch.Name == spec.Name {
			fail(w, errors.New("that model name is already used by a managed server"))
			return
		}
	}
	// These arguments belong to the manager so health checks and displayed model stay accurate.
	blocked := map[string]bool{"-m": true, "--model": true, "-a": true, "--alias": true, "--host": true, "--port": true, "-hf": true, "--hf-repo": true, "--models-preset": true, "--models-dir": true}
	for _, arg := range spec.Args {
		if blocked[strings.SplitN(arg, "=", 2)[0]] {
			fail(w, fmt.Errorf("%s is managed by llama-desk", arg))
			return
		}
	}
	for key, value := range spec.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			fail(w, errors.New("invalid environment variable"))
			return
		}
		if strings.HasPrefix(key, "LLAMA_ARG_") {
			fail(w, errors.New("use launch arguments instead of LLAMA_ARG_* overrides"))
			return
		}
	}
	args := []string{"--model", model, "--ctx-size", strconv.Itoa(spec.Context), "--n-gpu-layers", strconv.Itoa(spec.GPULayers)}
	args = append(args, spec.Args...)
	args = append(args, "--alias", spec.Name, "--host", "127.0.0.1", "--port", strconv.Itoa(spec.Port))
	ctx, stop := context.WithCancel(a.ctx)
	cmd := exec.CommandContext(ctx, a.binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	// Remove inherited llama.cpp settings and replace environment keys deterministically.
	env := map[string]string{}
	for _, pair := range os.Environ() {
		k, v, _ := strings.Cut(pair, "=")
		if !strings.HasPrefix(k, "LLAMA_ARG_") {
			env[k] = v
		}
	}
	for k, v := range spec.Env {
		env[k] = v
	}
	keys := []string{}
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	run := &Run{ID: id(), Launch: spec, Command: append([]string{a.binary}, args...), Status: "running", Started: time.Now(), stop: stop, ctx: ctx, log: &logBuffer{}}
	cmd.Stdout = run.log
	cmd.Stderr = run.log
	if err = cmd.Start(); err != nil {
		stop()
		fail(w, err)
		return
	}
	run.PID = cmd.Process.Pid
	a.runs[run.ID] = run
	a.profiles[spec.Model] = spec
	if err = a.save(); err != nil {
		run.Error = "Launch settings could not be saved: " + err.Error()
	}
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		// Ensure descendants are also killed when graceful shutdown expires.
		finished := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				select {
				case <-timer.C:
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				case <-finished:
				}
			case <-finished:
			}
		}()
		err := cmd.Wait()
		close(finished)
		a.mu.Lock()
		defer a.mu.Unlock()
		now := time.Now()
		run.Ended = &now
		run.Status = "stopped"
		run.stop = nil
		if err != nil && ctx.Err() == nil {
			run.Status = "failed"
			run.Error = err.Error()
		}
		stop()
	}()
	respond(w, 201, run)
}

func (a *App) stopRun(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	run := a.runs[r.PathValue("id")]
	if run == nil {
		http.NotFound(w, r)
		return
	}
	if run.stop != nil {
		run.Status = "stopping"
		run.stop()
	}
	respond(w, 202, map[string]bool{"accepted": true})
}
func (a *App) logs(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	run := a.runs[r.PathValue("id")]
	a.mu.Unlock()
	if run == nil {
		http.NotFound(w, r)
		return
	}
	respond(w, 200, map[string]string{"logs": run.log.String()})
}
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	run := a.runs[r.PathValue("id")]
	if run == nil {
		a.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	port, status := run.Launch.Port, run.Status
	a.mu.Unlock()
	if status != "running" {
		respond(w, 200, map[string]string{"status": status})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
	defer cancel()
	code, body, err := getJSON(a.client, ctx, fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		respond(w, 200, map[string]string{"status": "unreachable", "detail": err.Error()})
		return
	}
	respond(w, 200, map[string]any{"http_status": code, "response": body})
}
