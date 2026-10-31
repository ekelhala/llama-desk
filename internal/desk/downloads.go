package desk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Job struct {
	ID             string    `json:"id"`
	URL            string    `json:"url"`
	File           string    `json:"file"`
	Status         string    `json:"status"`
	Bytes          int64     `json:"bytes"`
	Total          int64     `json:"total"`
	BytesPerSecond float64   `json:"bytes_per_second"`
	Error          string    `json:"error,omitempty"`
	ETag           string    `json:"etag,omitempty"`
	Created        time.Time `json:"created"`
	cancel         context.CancelFunc
}

var repoPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Build the file URLs on the server so users only need a repository and filenames.
func repositoryURLs(repo, revision string, files []string) ([]string, error) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) != 2 || !repoPart.MatchString(parts[0]) || !repoPart.MatchString(parts[1]) {
		return nil, errors.New("enter the Hugging Face repository as owner/name")
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		revision = "main"
	}
	if !repoPart.MatchString(revision) {
		return nil, errors.New("revision must be a branch name, tag or commit without slashes")
	}
	if len(files) < 1 || len(files) > 32 {
		return nil, errors.New("provide between 1 and 32 filenames")
	}
	urls := make([]string, 0, len(files))
	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" || strings.HasPrefix(file, "/") || strings.ContainsAny(file, "*?[]") {
			return nil, errors.New("enter an exact GGUF filename, including any subdirectory")
		}
		u := url.URL{Scheme: "https", Host: "huggingface.co", Path: "/" + strings.Join(parts, "/") + "/resolve/" + revision + "/" + file}
		if _, err := downloadTarget(u.String()); err != nil {
			return nil, err
		}
		urls = append(urls, u.String())
	}
	return urls, nil
}

// Also validates URLs from previously saved jobs and older API clients.
func downloadTarget(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" || u.Host != "huggingface.co" || u.User != nil {
		return "", errors.New("use an https://huggingface.co/.../resolve/.../file.gguf URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 5 || parts[2] != "resolve" {
		return "", errors.New("paste the file's Hugging Face download URL (containing /resolve/)")
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.Contains(p, "\\") {
			return "", errors.New("invalid download path")
		}
	}
	if !strings.HasSuffix(strings.ToLower(parts[len(parts)-1]), ".gguf") {
		return "", errors.New("only GGUF files are supported")
	}
	// Include the repo and revision, avoiding collisions between similarly named models.
	return path.Join(append(parts[:2], parts[3:]...)...), nil
}

func (a *App) addDownload(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Repo     string   `json:"repo"`
		Files    []string `json:"files"`
		Revision string   `json:"revision"`
		URLs     []string `json:"urls"` // Compatibility with existing clients and saved jobs.
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, err)
		return
	}
	if input.Repo != "" || len(input.Files) > 0 || input.Revision != "" {
		if len(input.URLs) > 0 {
			fail(w, errors.New("provide repo and files, or URLs, not both"))
			return
		}
		var err error
		input.URLs, err = repositoryURLs(input.Repo, input.Revision, input.Files)
		if err != nil {
			fail(w, err)
			return
		}
	}
	if len(input.URLs) < 1 || len(input.URLs) > 32 {
		fail(w, errors.New("provide a repository and at least one GGUF filename"))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	jobs := []*Job{}
	targets := map[string]bool{}
	for _, raw := range input.URLs {
		raw = strings.TrimSpace(raw)
		file, err := downloadTarget(raw)
		if err != nil {
			fail(w, err)
			return
		}
		if targets[file] {
			fail(w, errors.New("duplicate destination in download list"))
			return
		}
		targets[file] = true
		for _, j := range a.jobs {
			if j.File == file && j.Status != "completed" {
				fail(w, errors.New("this file already has a job; use Resume on that job"))
				return
			}
		}
		if _, err = os.Lstat(filepath.Join(a.models, filepath.FromSlash(file))); !os.IsNotExist(err) {
			fail(w, errors.New("destination already exists or cannot be inspected"))
			return
		}
		jobs = append(jobs, &Job{ID: id(), URL: raw, File: file, Status: "queued", Created: time.Now()})
	}
	for _, j := range jobs {
		a.jobs[j.ID] = j
	}
	if err := a.save(); err != nil {
		for _, j := range jobs {
			delete(a.jobs, j.ID)
		}
		fail(w, err)
		return
	}
	for _, j := range jobs {
		a.startJob(j)
	}
	respond(w, 202, jobs)
}

// Caller holds a.mu; workers also acquire it before reading mutable job fields.
func (a *App) startJob(j *Job) {
	ctx, cancel := context.WithCancel(a.ctx)
	j.cancel = cancel
	j.Status = "queued"
	j.Error = ""
	j.BytesPerSecond = 0
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		select {
		case a.slots <- struct{}{}:
			defer func() { <-a.slots }()
		case <-ctx.Done():
			a.finishJob(j, ctx.Err())
			return
		}
		a.mu.Lock()
		j.Status = "downloading"
		a.mu.Unlock()
		err := a.transfer(ctx, j)
		a.finishJob(j, err)
	}()
}

func (a *App) finishJob(j *Job, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j.cancel = nil
	j.BytesPerSecond = 0
	if err == nil {
		j.Status = "completed"
	} else if errors.Is(err, context.Canceled) {
		j.Status = "cancelled"
		j.Error = "Paused; click Resume to continue."
	} else {
		j.Status = "failed"
		j.Error = err.Error()
	}
	if saveErr := a.save(); saveErr != nil {
		j.Error = "Could not save job history: " + saveErr.Error()
	}
}

func (a *App) jobAction(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j := a.jobs[r.PathValue("id")]
	if j == nil {
		http.NotFound(w, r)
		return
	}
	switch r.PathValue("action") {
	case "cancel":
		if j.cancel != nil {
			j.cancel()
		}
	case "resume":
		if j.cancel != nil || j.Status == "completed" {
			fail(w, errors.New("only interrupted, failed or cancelled jobs can resume"))
			return
		}
		if _, err := downloadTarget(j.URL); err != nil {
			fail(w, err)
			return
		}
		a.startJob(j)
	default:
		http.NotFound(w, r)
		return
	}
	respond(w, 202, map[string]bool{"accepted": true})
}

func (a *App) transfer(ctx context.Context, j *Job) error {
	// os.Root confines writes even when a symlink appears beneath the model directory.
	root, err := os.OpenRoot(a.models)
	if err != nil {
		return err
	}
	defer root.Close()
	relative := filepath.FromSlash(j.File)
	// Workers commonly share repository directories. Serialize mkdir operations:
	// os.Root.MkdirAll can return EEXIST when another worker creates a parent.
	a.mu.Lock()
	err = root.MkdirAll(filepath.Dir(relative), 0750)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err = root.Stat(relative); !os.IsNotExist(err) {
		return errors.New("destination already exists; refusing to overwrite")
	}
	partial := relative + ".part"
	file, err := root.OpenFile(partial, os.O_CREATE|os.O_RDWR, 0640)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	offset := info.Size()
	a.mu.Lock()
	etag := j.ETag
	a.mu.Unlock()
	// A partial file without a persisted validator cannot safely resume a mutable URL.
	if etag == "" {
		offset = 0
	}
	req, err := http.NewRequestWithContext(ctx, "GET", j.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		req.Header.Set("If-Range", etag)
	}
	response, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("Hugging Face returned HTTP %d", response.StatusCode)
	}
	total := response.ContentLength
	if response.StatusCode == http.StatusPartialContent {
		var start, end, length int64
		if _, err = fmt.Sscanf(response.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &length); err != nil || start != offset || end < start || length <= end {
			return errors.New("invalid resume response; partial file retained")
		}
		total = length
	} else {
		offset = 0
		if err = file.Truncate(0); err != nil {
			return err
		}
	}
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	a.mu.Lock()
	j.Bytes = offset
	j.Total = total
	j.ETag = response.Header.Get("ETag")
	err = a.save()
	a.mu.Unlock()
	if err != nil {
		return err
	}
	buffer := make([]byte, 256<<10)
	started := time.Now()
	var downloaded int64
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			written, writeErr := file.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			downloaded += int64(n)
			a.mu.Lock()
			j.Bytes = offset + downloaded
			j.BytesPerSecond = float64(downloaded) / time.Since(started).Seconds()
			a.mu.Unlock()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if total >= 0 && offset+downloaded != total {
		return errors.New("incomplete download; partial file retained")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	magic := make([]byte, 4)
	if _, err = io.ReadFull(file, magic); err != nil {
		return err
	}
	if string(magic) != "GGUF" {
		return errors.New("download is not a GGUF; partial file retained")
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	// Link publishes atomically without replacing any existing file.
	if err = root.Link(partial, relative); err != nil {
		return err
	}
	return root.Remove(partial)
}

// Kept separate for readable progress in the UI and HTTP tests.
func parsePort(port int) error {
	if port < 1024 || port > 65535 {
		return errors.New("port must be between " + strconv.Itoa(1024) + " and 65535")
	}
	return nil
}
