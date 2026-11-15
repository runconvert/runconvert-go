package runconvert

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live tests against the real API: go test -run Live with RUNCONVERT_API_KEY set
// (converts one small text file, about 2 credits).
func liveClient(t *testing.T) *Client {
	if os.Getenv("RUNCONVERT_API_KEY") == "" {
		t.Skip("set RUNCONVERT_API_KEY to run live tests")
	}
	return New("")
}

func TestLiveMeAndOperations(t *testing.T) {
	rc := liveClient(t)
	ctx := context.Background()
	if me, err := rc.Me(ctx); err != nil || me.Credits == nil {
		t.Fatalf("me: %+v %v", me, err)
	}
	in, err := rc.Operations.ForInput(ctx, "TXT")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range in.Outputs {
		found = found || o.Format == "pdf"
	}
	if !found {
		t.Fatal("txt has no pdf target")
	}
	if pair, err := rc.Operations.ForPair(ctx, "mp4", "mp3"); err != nil || len(pair.Engines) == 0 {
		t.Fatalf("pair: %v", err)
	}
}

func TestLiveConvertTextToPDF(t *testing.T) {
	rc := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	dir := t.TempDir()
	src := filepath.Join(dir, "sdk-live-test.txt")
	_ = os.WriteFile(src, []byte("RunConvert Go SDK live test\nLine two\n\nCafé – 你好 – Привет\n"), 0o644)
	var seen []string
	res, err := rc.Convert(ctx, src, ConvertOptions{To: "pdf", OutDir: filepath.Join(dir, "out"), Tag: "sdk-go-live-test", OnUpdate: func(j *Job) {
		if len(seen) == 0 || seen[len(seen)-1] != j.Status {
			seen = append(seen, j.Status)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	pdf, _ := os.ReadFile(res.Saved[0])
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("not a PDF: %q", pdf[:min(len(pdf), 20)])
	}
	t.Logf("job %s: %s, %d credit(s), %d bytes", res.Job.ID, strings.Join(seen, " → "), res.Job.CreditsUsed, len(pdf))
}

func TestLiveBadKey(t *testing.T) {
	liveClient(t)
	_, err := New("rc_live_invalid", WithMaxRetries(0)).Me(context.Background())
	if e, ok := err.(*Error); !ok || e.Status != 401 {
		t.Fatalf("bad key: %v", err)
	}
}
