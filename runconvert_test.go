package runconvert

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func ok(w http.ResponseWriter, data any, meta any) {
	body := map[string]any{"success": true, "data": data}
	if meta != nil {
		body["meta"] = meta
	}
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": map[string]string{"code": code, "message": msg}})
}

func fake(t *testing.T, h http.HandlerFunc) *Client {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New("rc_live_test", WithBaseURL(srv.URL+"/v3"), WithMaxRetries(2))
}

func TestKeyAndEnvelope(t *testing.T) {
	var auth, ua string
	rc := fake(t, func(w http.ResponseWriter, r *http.Request) {
		auth, ua = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		ok(w, map[string]any{"credits": map[string]any{"mode": "wallet", "total_remaining": 42}}, nil)
	})
	me, err := rc.Me(context.Background())
	if err != nil || me.Credits.TotalRemaining != 42 {
		t.Fatalf("me = %+v, %v", me, err)
	}
	if auth != "Bearer rc_live_test" || !strings.HasPrefix(ua, "runconvert-go/") {
		t.Fatalf("headers %q %q", auth, ua)
	}
}

func TestAPIError(t *testing.T) {
	rc := fake(t, func(w http.ResponseWriter, r *http.Request) {
		fail(w, 422, "BILLING_INSUFFICIENT_CREDITS", "Insufficient credits.")
	})
	_, err := rc.Jobs.Get(context.Background(), "job_1")
	var e *Error
	if !errors.As(err, &e) || e.Status != 422 || e.Code != "BILLING_INSUFFICIENT_CREDITS" || e.Message != "Insufficient credits." {
		t.Fatalf("err = %#v", err)
	}
}

func TestRetries(t *testing.T) {
	var n int32
	rc := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(503)
			return
		}
		ok(w, map[string]any{"id": "job_1", "status": "finished"}, nil)
	})
	if job, err := rc.Jobs.Get(context.Background(), "job_1"); err != nil || job.Status != "finished" || n != 3 {
		t.Fatalf("job %+v err %v calls %d", job, err, n)
	}

	var creates int32
	rc2 := fake(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&creates, 1)
		fail(w, 500, "INTERNAL", "boom")
	})
	if _, err := rc2.Jobs.Create(context.Background(), JobCreate{}); err == nil || creates != 1 {
		t.Fatalf("POST /jobs retried after 500: calls %d err %v", creates, err)
	}

	var limited int32
	rc3 := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&limited, 1) == 1 {
			fail(w, 429, "ACTIVE_JOB_LIMIT_REACHED", "x")
			return
		}
		if r.URL.Query().Get("include") != "tasks" {
			t.Errorf("create without include=tasks")
		}
		ok(w, map[string]any{"id": "job_1", "status": "waiting"}, nil)
	})
	if job, err := rc3.Jobs.Create(context.Background(), JobCreate{}); err != nil || job.ID != "job_1" || limited != 2 {
		t.Fatalf("429 retry: %v %d", err, limited)
	}
}

func TestListAndAll(t *testing.T) {
	var first *http.Request
	rc := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if first == nil {
			first = r
		}
		p := r.URL.Query().Get("page")
		ok(w, []map[string]any{{"id": "job_p" + p, "status": "finished"}}, map[string]any{"page": 1, "total_pages": 2})
	})
	_, meta, err := rc.Jobs.List(context.Background(), ListParams{Status: "finished", Tag: "x", Page: 1, PerPage: 1})
	if err != nil || meta.TotalPages != 2 || first.URL.Query().Get("filter[status]") != "finished" || first.URL.Query().Get("filter[tag]") != "x" {
		t.Fatalf("list: %v %+v %v", err, meta, first.URL.RawQuery)
	}
	var ids []string
	_ = rc.Jobs.All(context.Background(), ListParams{PerPage: 1}, func(j *Job) bool { ids = append(ids, j.ID); return true })
	if strings.Join(ids, ",") != "job_p1,job_p2" {
		t.Fatalf("all = %v", ids)
	}
}

func TestWait(t *testing.T) {
	var n int32
	states := []string{"waiting", "processing", "finished"}
	rc := fake(t, func(w http.ResponseWriter, r *http.Request) {
		i := min(int(atomic.AddInt32(&n, 1))-1, 2)
		ok(w, map[string]any{"id": "job_1", "status": states[i]}, nil)
	})
	var seen []string
	job, err := rc.Jobs.Wait(context.Background(), "job_1", WaitOptions{OnUpdate: func(j *Job) { seen = append(seen, j.Status) }})
	if err != nil || job.Status != "finished" || strings.Join(seen, ",") != "waiting,processing,finished" {
		t.Fatalf("wait: %v %v", err, seen)
	}

	rcFail := fake(t, func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"id": "job_1", "status": "error", "tasks": []map[string]any{{"name": "c", "operation": "convert", "status": "error", "code": "INVALID_DATA", "reason": "INPUT_EMPTY", "message": "The file is empty."}}}, nil)
	})
	_, err = rcFail.Jobs.Wait(context.Background(), "job_1", WaitOptions{})
	var jf *JobFailedError
	if !errors.As(err, &jf) || jf.Task.Reason != "INPUT_EMPTY" || err.Error() != "runconvert: The file is empty." {
		t.Fatalf("failed job: %v", err)
	}
	if job, err := rcFail.Jobs.Wait(context.Background(), "job_1", WaitOptions{NoErrorOnFail: true}); err != nil || job.Status != "error" {
		t.Fatalf("NoErrorOnFail: %v", err)
	}

	rcSlow := fake(t, func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"id": "job_1", "status": "processing"}, nil)
	})
	if _, err := rcSlow.Jobs.Wait(context.Background(), "job_1", WaitOptions{Timeout: time.Millisecond}); !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("timeout: %v", err)
	}
}

func TestDeleteAndOperations(t *testing.T) {
	var q string
	rc := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		q = r.URL.RawQuery
		ok(w, map[string]any{"input": "mp4", "output": "mp3", "engines": []any{}}, nil)
	})
	if err := rc.Jobs.Delete(context.Background(), "job_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Operations.ForPair(context.Background(), "MP4", "MP3"); err != nil || q != "input=mp4&output=mp3" {
		t.Fatalf("ForPair: %v %q", err, q)
	}
}

func TestWebhooks(t *testing.T) {
	body := []byte(`{"event":"job.finished","job":{"id":"job_1","status":"finished"}}`)
	mac := hmac.New(sha256.New, []byte("whsec_x"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	cases := []struct {
		body []byte
		sig  string
		key  string
		want bool
	}{
		{body, sig, "whsec_x", true},
		{append(body, ' '), sig, "whsec_x", false},
		{body, sig, "other", false},
		{body, "sha256=nothex", "whsec_x", false},
		{body, "", "whsec_x", false},
	}
	for i, c := range cases {
		if got := VerifySignature(c.body, c.sig, c.key); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
	ev, err := ConstructEvent(body, sig, "whsec_x")
	if err != nil || ev.Job.ID != "job_1" {
		t.Fatalf("ConstructEvent: %v", err)
	}
	if _, err := ConstructEvent(body, "sha256="+strings.Repeat("0", 64), "whsec_x"); err == nil {
		t.Fatal("bad signature accepted")
	}
}

func TestConvertUploadWaitDownload(t *testing.T) {
	var created map[string]any
	var putName, putToken, putBody string
	var polls int32
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v3/jobs":
			_ = json.NewDecoder(r.Body).Decode(&created)
			ok(w, map[string]any{"id": "job_1", "status": "waiting", "tasks": []map[string]any{{"name": "import-1", "operation": "import/direct/upload", "status": "waiting", "upload_url": srvURL + "/upload?token=t"}}}, nil)
		case r.Method == http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			putName, putToken, putBody = r.URL.Query().Get("filename"), r.URL.Query().Get("token"), string(b)
		case r.URL.Path == "/v3/jobs/job_1":
			status := "processing"
			if atomic.AddInt32(&polls, 1) > 1 {
				status = "finished"
			}
			ok(w, map[string]any{"id": "job_1", "status": status, "tasks": []map[string]any{{"name": "export-1", "operation": "export/url", "status": "finished", "result": map[string]any{"files": []map[string]any{{"filename": "notes.pdf", "url": srvURL + "/dl/notes.pdf"}}}}}}, nil)
		case r.URL.Path == "/dl/notes.pdf":
			_, _ = w.Write([]byte("PDF"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	rc := New("rc_live_test", WithBaseURL(srv.URL+"/v3"))
	dir := t.TempDir()
	src := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(src, []byte("hello"), 0o644)
	res, err := rc.Convert(context.Background(), src, ConvertOptions{To: "pdf", OutDir: filepath.Join(dir, "out"), Options: map[string]any{"page_size": "a4"}})
	if err != nil {
		t.Fatal(err)
	}
	conv := created["tasks"].(map[string]any)["convert-1"].(map[string]any)
	if conv["input_format"] != "txt" || conv["output_format"] != "pdf" || conv["page_size"] != "a4" || conv["input"] != "import-1" {
		t.Fatalf("convert task = %v", conv)
	}
	if putName != "notes.txt" || putToken != "t" || putBody != "hello" {
		t.Fatalf("upload: %q %q %q", putName, putToken, putBody)
	}
	if b, _ := os.ReadFile(res.Saved[0]); string(b) != "PDF" {
		t.Fatalf("download = %q", b)
	}
}

func TestConvertURLAndBadInput(t *testing.T) {
	var tasks map[string]any
	rc := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			tasks = body["tasks"].(map[string]any)
		}
		ok(w, map[string]any{"id": "job_2", "status": "finished"}, nil)
	})
	if _, err := rc.Convert(context.Background(), "https://example.com/a/talk.MP4?x=1", ConvertOptions{To: "mp3"}); err != nil {
		t.Fatal(err)
	}
	imp := tasks["import-1"].(map[string]any)
	if imp["operation"] != "import/url" || tasks["convert-1"].(map[string]any)["input_format"] != "mp4" {
		t.Fatalf("tasks = %v", tasks)
	}
	if _, err := rc.Convert(context.Background(), strings.NewReader("x"), ConvertOptions{To: "pdf"}); err == nil {
		t.Fatal("reader without filename accepted")
	}
}
