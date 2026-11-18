// Package runconvert is the official Go SDK for the RunConvert API (v3): convert files
// between 240+ formats.
//
//	rc := runconvert.New("") // reads RUNCONVERT_API_KEY
//	res, err := rc.Convert(ctx, "report.docx", runconvert.ConvertOptions{To: "pdf", OutDir: "out"})
package runconvert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Version of this SDK.
const Version = "0.1.0" // x-release-please-version

// DefaultBaseURL is the production API.
const DefaultBaseURL = "https://api.runconvert.com/v3"

// Client talks to the RunConvert API. Create one with New and reuse it.
type Client struct {
	apiKey     string
	baseURL    string
	maxRetries int
	http       *http.Client // API calls, with a timeout
	transfer   *http.Client // uploads and downloads, no timeout
	userAgent  string

	Jobs       *JobsService
	Operations *OperationsService
	Files      *FilesService
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at another API (tests, staging).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }

// WithHTTPClient replaces the HTTP client used for API calls and transfers.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http, c.transfer = h, h }
}

// WithTimeout sets the per-request timeout for API calls (default 60s). Uploads and downloads have none.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http = &http.Client{Timeout: d} }
}

// WithMaxRetries sets retries for network errors, 429 and 5xx (default 2).
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// New creates a client. An empty apiKey reads RUNCONVERT_API_KEY.
func New(apiKey string, opts ...Option) *Client {
	if apiKey == "" {
		apiKey = os.Getenv("RUNCONVERT_API_KEY")
	}
	c := &Client{
		apiKey:     apiKey,
		baseURL:    DefaultBaseURL,
		maxRetries: 2,
		http:       &http.Client{Timeout: 60 * time.Second},
		transfer:   &http.Client{},
		userAgent:  fmt.Sprintf("runconvert-go/%s %s", Version, runtime.Version()),
	}
	for _, o := range opts {
		o(c)
	}
	c.Jobs = &JobsService{c}
	c.Operations = &OperationsService{c}
	c.Files = &FilesService{c}
	return c
}

// ---- errors ----

// Error is an API error: {"success": false, "error": {"code", "message"}}.
// Code is machine-readable (e.g. BILLING_INSUFFICIENT_CREDITS); new codes can appear.
type Error struct {
	Status  int    // HTTP status, 0 when there was no response
	Code    string // API error code, may be empty
	Message string
	Body    []byte
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("runconvert: %s (%s, HTTP %d)", e.Message, e.Code, e.Status)
	}
	return "runconvert: " + e.Message
}

// JobFailedError: the job ended with status "error". Task is the first failed task.
type JobFailedError struct {
	Job  *Job
	Task *Task
}

func (e *JobFailedError) Error() string {
	if e.Task != nil && e.Task.Message != "" {
		return "runconvert: " + e.Task.Message
	}
	return "runconvert: job " + e.Job.ID + " failed"
}

// ErrWaitTimeout is returned (wrapped) when Wait gives up.
var ErrWaitTimeout = errors.New("runconvert: timed out waiting for the job")

// ---- HTTP ----

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Meta    *ListMeta       `json:"meta"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

var retryStatus = map[int]bool{408: true, 425: true, 429: true, 500: true, 502: true, 503: true, 504: true}

// do sends a request and decodes "data" into out (and "meta" into meta when given).
func (c *Client) do(ctx context.Context, method, p string, query url.Values, body, out any, meta *ListMeta) error {
	u := c.baseURL + p
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	// POST /jobs is only repeated when it never reached the server: a job is never created twice.
	idempotent := method != http.MethodPost || strings.HasSuffix(p, "/cancel")
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ne interface{ Timeout() bool }
			reached := errors.As(err, &ne) && ne.Timeout()
			if attempt < c.maxRetries && (idempotent || !reached) {
				if sleepCtx(ctx, backoff(attempt)) != nil {
					return ctx.Err()
				}
				continue
			}
			return &Error{Message: "request failed: " + err.Error()}
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if rerr != nil {
			return rerr
		}
		if retryStatus[resp.StatusCode] && attempt < c.maxRetries && (idempotent || resp.StatusCode == 429) {
			wait := backoff(attempt)
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = time.Duration(min(s, 30)) * time.Second
			}
			if sleepCtx(ctx, wait) != nil {
				return ctx.Err()
			}
			continue
		}
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		var env envelope
		jerr := json.Unmarshal(raw, &env)
		if resp.StatusCode >= 400 || jerr != nil || !env.Success {
			e := &Error{Status: resp.StatusCode, Message: fmt.Sprintf("API error (HTTP %d)", resp.StatusCode), Body: raw}
			if env.Error != nil {
				e.Code, e.Message = env.Error.Code, env.Error.Message
			}
			return e
		}
		if meta != nil && env.Meta != nil {
			*meta = *env.Meta
		}
		if out != nil && len(env.Data) > 0 {
			return json.Unmarshal(env.Data, out)
		}
		return nil
	}
}

func backoff(attempt int) time.Duration {
	d := time.Duration(500*(1<<attempt)) * time.Millisecond
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return time.Duration(float64(d) * (0.75 + rand.Float64()/2))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- account ----

// Me returns the account, team and credit balance behind the API key.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var me Me
	return &me, c.do(ctx, http.MethodGet, "/me", nil, nil, &me, nil)
}

// ---- convert ----

// ConvertOptions for Convert.
type ConvertOptions struct {
	To       string         // target format, e.g. "mp3" (required)
	From     string         // source format; read from the file name or URL when empty
	Options  map[string]any // engine options, e.g. {"audioBitrate": "192k"}
	OutDir   string         // save results here (created if needed); empty: don't download
	Filename string         // file name for an io.Reader input (also sets From)
	Tag      string
	OnUpdate func(*Job)
}

// ConvertResult of Convert.
type ConvertResult struct {
	Job   *Job
	Files []File   // the converted files, with temporary download links
	Saved []string // local paths, when OutDir was set
}

// Convert converts one file in one call: upload (or import a URL), convert, wait, download.
// input is a local path, an http(s) URL, or an io.Reader (then set Filename or From).
// Use ctx for a time limit.
func (c *Client) Convert(ctx context.Context, input any, o ConvertOptions) (*ConvertResult, error) {
	var name, sourceURL string
	switch v := input.(type) {
	case string:
		if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
			sourceURL = v
			if pu, err := url.Parse(v); err == nil {
				name = path.Base(pu.Path)
			}
		} else {
			name = v
		}
	case io.Reader:
	default:
		return nil, fmt.Errorf("runconvert: input must be a path, a URL or an io.Reader, got %T", input)
	}
	if o.Filename != "" {
		name = o.Filename
	}
	from := strings.ToLower(o.From)
	if from == "" {
		from = strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	}
	if from == "" {
		return nil, errors.New(`runconvert: can't tell the source format: set From ("mp4") or Filename`)
	}
	convert := ConvertTask(from, strings.ToLower(strings.TrimPrefix(o.To, ".")), "import-1", o.Options)
	imp := ImportUpload()
	if sourceURL != "" {
		imp = ImportURL(sourceURL)
	}
	job, err := c.Jobs.Create(ctx, JobCreate{Tag: o.Tag, Tasks: map[string]TaskDefinition{
		"import-1": imp, "convert-1": convert, "export-1": ExportURL("convert-1"),
	}})
	if err != nil {
		return nil, err
	}
	if sourceURL == "" {
		task := job.Task("import-1")
		if task == nil || task.UploadURL == "" {
			return nil, &Error{Message: "the job has no upload URL"}
		}
		if err := c.Files.Upload(ctx, task.UploadURL, input, filepath.Base(name)); err != nil {
			return nil, err
		}
	}
	job, err = c.Jobs.Wait(ctx, job.ID, WaitOptions{OnUpdate: o.OnUpdate})
	if err != nil {
		return nil, err
	}
	res := &ConvertResult{Job: job, Files: job.ExportedFiles()}
	if o.OutDir != "" {
		for _, f := range res.Files {
			p, err := c.Files.Download(ctx, f.URL, o.OutDir, f.Filename)
			if err != nil {
				return res, err
			}
			res.Saved = append(res.Saved, p)
		}
	}
	return res, nil
}
