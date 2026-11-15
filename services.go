package runconvert

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---- jobs ----

// JobsService creates, reads, waits for and stops jobs.
type JobsService struct{ c *Client }

// Create creates a job. It comes back with its tasks (an upload task carries UploadURL).
func (s *JobsService) Create(ctx context.Context, body JobCreate) (*Job, error) {
	var job Job
	return &job, s.c.do(ctx, http.MethodPost, "/jobs", url.Values{"include": {"tasks"}}, body, &job, nil)
}

// Get returns a job with its tasks.
func (s *JobsService) Get(ctx context.Context, id string) (*Job, error) {
	var job Job
	return &job, s.c.do(ctx, http.MethodGet, "/jobs/"+url.PathEscape(id), url.Values{"include": {"tasks"}}, nil, &job, nil)
}

// ListParams filters Jobs.List.
type ListParams struct {
	Status       string
	Tag          string
	Page         int
	PerPage      int
	IncludeTasks bool
}

// List returns one page of jobs, newest first.
func (s *JobsService) List(ctx context.Context, p ListParams) ([]*Job, ListMeta, error) {
	q := url.Values{}
	if p.Status != "" {
		q.Set("filter[status]", p.Status)
	}
	if p.Tag != "" {
		q.Set("filter[tag]", p.Tag)
	}
	if p.Page > 0 {
		q.Set("page", strconv.Itoa(p.Page))
	}
	if p.PerPage > 0 {
		q.Set("per_page", strconv.Itoa(p.PerPage))
	}
	if p.IncludeTasks {
		q.Set("include", "tasks")
	}
	var jobs []*Job
	var meta ListMeta
	err := s.c.do(ctx, http.MethodGet, "/jobs", q, nil, &jobs, &meta)
	return jobs, meta, err
}

// All calls fn for every job, page by page; return false from fn to stop.
func (s *JobsService) All(ctx context.Context, p ListParams, fn func(*Job) bool) error {
	for page := 1; ; page++ {
		p.Page = page
		jobs, meta, err := s.List(ctx, p)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if !fn(j) {
				return nil
			}
		}
		if len(jobs) == 0 || (meta.TotalPages > 0 && page >= meta.TotalPages) {
			return nil
		}
	}
}

// Cancel cancels pending tasks; credits for unfinished tasks are refunded.
func (s *JobsService) Cancel(ctx context.Context, id string) (*Job, error) {
	var job Job
	return &job, s.c.do(ctx, http.MethodPost, "/jobs/"+url.PathEscape(id)+"/cancel", nil, nil, &job, nil)
}

// Delete deletes a job.
func (s *JobsService) Delete(ctx context.Context, id string) error {
	return s.c.do(ctx, http.MethodDelete, "/jobs/"+url.PathEscape(id), nil, nil, nil, nil)
}

// WaitOptions for Wait.
type WaitOptions struct {
	Timeout       time.Duration // 0: no limit besides ctx
	OnUpdate      func(*Job)
	NoErrorOnFail bool // return a failed job instead of *JobFailedError
}

// Wait polls until the job is finished or error (0.7s, slowing to 3s between polls).
// A failed job returns *JobFailedError unless NoErrorOnFail.
func (s *JobsService) Wait(ctx context.Context, id string, o WaitOptions) (*Job, error) {
	var deadline time.Time
	if o.Timeout > 0 {
		deadline = time.Now().Add(o.Timeout)
	}
	delay := 700 * time.Millisecond
	for {
		job, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if o.OnUpdate != nil {
			o.OnUpdate(job)
		}
		switch job.Status {
		case StatusFinished:
			return job, nil
		case StatusError:
			if o.NoErrorOnFail {
				return job, nil
			}
			var failed *Task
			for _, t := range job.Tasks {
				if t.Status == StatusError {
					failed = t
					break
				}
			}
			return job, &JobFailedError{Job: job, Task: failed}
		}
		if !deadline.IsZero() && time.Now().Add(delay).After(deadline) {
			return job, fmt.Errorf("%w: job %s still %s", ErrWaitTimeout, job.ID, job.Status)
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return job, err
		}
		if delay < 3*time.Second {
			delay += 300 * time.Millisecond
		}
	}
}

// ---- operations ----

// OperationsService lists formats, targets, engines and options. Public: no key needed.
type OperationsService struct{ c *Client }

// List returns every input format with its targets (category "" for all).
func (s *OperationsService) List(ctx context.Context, category string) (*OperationsCatalog, error) {
	q := url.Values{}
	if category != "" {
		q.Set("category", category)
	}
	var out OperationsCatalog
	return &out, s.c.do(ctx, http.MethodGet, "/operations", q, nil, &out, nil)
}

// ForInput returns what one format converts to, with credit costs.
func (s *OperationsService) ForInput(ctx context.Context, input string) (*InputFormat, error) {
	var out InputFormat
	return &out, s.c.do(ctx, http.MethodGet, "/operations", url.Values{"input": {strings.ToLower(input)}}, nil, &out, nil)
}

// ForPair returns the engines for input → output and the options a convert task accepts.
func (s *OperationsService) ForPair(ctx context.Context, input, output string) (*PairEngines, error) {
	var out PairEngines
	q := url.Values{"input": {strings.ToLower(input)}, "output": {strings.ToLower(output)}}
	return &out, s.c.do(ctx, http.MethodGet, "/operations", q, nil, &out, nil)
}

// ---- files ----

// FilesService uploads to import/direct/upload tasks and downloads results.
type FilesService struct{ c *Client }

// Upload PUTs a file to a task's upload URL before its deadline. file is a local path or an
// io.Reader; filename (optional) keeps the file's name.
func (s *FilesService) Upload(ctx context.Context, uploadURL string, file any, filename string) error {
	if uploadURL == "" {
		return errors.New("runconvert: empty upload URL (is it an import/direct/upload task?)")
	}
	u, err := url.Parse(uploadURL)
	if err != nil {
		return err
	}
	var body io.Reader
	var size int64 = -1
	switch v := file.(type) {
	case string:
		f, err := os.Open(v)
		if err != nil {
			return err
		}
		defer f.Close()
		if st, err := f.Stat(); err == nil {
			size = st.Size()
		}
		body = f
		if filename == "" {
			filename = filepath.Base(v)
		}
	case io.Reader:
		body = v
	default:
		return fmt.Errorf("runconvert: file must be a path or an io.Reader, got %T", file)
	}
	if filename != "" {
		q := u.Query()
		q.Set("filename", filename)
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("User-Agent", s.c.userAgent)
	resp, err := s.c.transfer.Do(req)
	if err != nil {
		return &Error{Message: "upload failed: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &Error{Status: resp.StatusCode, Message: fmt.Sprintf("upload failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(b)))}
	}
	return nil
}

// Download saves a result file into dir (created if needed) and returns its path.
// filename "" uses the last part of the URL.
func (s *FilesService) Download(ctx context.Context, fileURL, dir, filename string) (string, error) {
	if filename == "" {
		if u, err := url.Parse(fileURL); err == nil {
			filename = filepath.Base(u.Path)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", s.c.userAgent)
	resp, err := s.c.transfer.Do(req)
	if err != nil {
		return "", &Error{Message: "download failed: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", &Error{Status: resp.StatusCode, Message: fmt.Sprintf("download failed (HTTP %d)", resp.StatusCode)}
	}
	dest := filepath.Join(dir, filepath.Base(filename))
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", err
	}
	return dest, f.Close()
}

// ---- webhooks ----

// VerifySignature checks a delivery's X-RunConvert-Signature header ("sha256=" + hex
// HMAC-SHA256 of the raw body, keyed with the endpoint's secret). Pass the body as received.
func VerifySignature(rawBody []byte, signature, secret string) bool {
	if signature == "" || secret == "" {
		return false
	}
	given := strings.ToLower(strings.TrimSpace(signature))
	given = strings.TrimPrefix(given, "sha256=")
	got, err := hex.DecodeString(given)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rawBody)
	return hmac.Equal(mac.Sum(nil), got)
}

// ConstructEvent verifies the signature, then parses the event.
func ConstructEvent(rawBody []byte, signature, secret string) (*WebhookEvent, error) {
	if !VerifySignature(rawBody, signature, secret) {
		return nil, errors.New("runconvert: invalid webhook signature")
	}
	var ev WebhookEvent
	return &ev, json.Unmarshal(rawBody, &ev)
}
