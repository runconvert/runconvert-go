# RunConvert SDK for Go

Convert files between 240+ formats (3,000+ conversions) with the [RunConvert API](https://www.runconvert.com/docs):
documents, images, video, audio, ebooks, archives, fonts and CAD. Go 1.21+, standard library only.

```bash
go get github.com/runconvert/runconvert-go
```

```go
import runconvert "github.com/runconvert/runconvert-go"

rc := runconvert.New("") // reads RUNCONVERT_API_KEY

// One call: upload, convert, wait, download.
res, err := rc.Convert(ctx, "report.docx", runconvert.ConvertOptions{To: "pdf", OutDir: "out"})
fmt.Println(res.Saved) // [out/report.pdf]
```

Get an API key in **Dashboard → API Keys** on [runconvert.com](https://www.runconvert.com). Keep keys on the server.

## Convert

```go
// From a URL (imported by RunConvert, nothing to upload)
rc.Convert(ctx, "https://example.com/talk.mp4", runconvert.ConvertOptions{To: "mp3", Options: map[string]any{"audioBitrate": "192k"}})

// From an io.Reader: give a Filename (or From)
rc.Convert(ctx, reader, runconvert.ConvertOptions{To: "jpg", Filename: "photo.heic", OutDir: "out"})

// Time limit: use the context
ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
defer cancel()
```

## Jobs with your own tasks

```go
// Merge three PDFs into one
job, err := rc.Jobs.Create(ctx, runconvert.JobCreate{Tag: "merge", Tasks: map[string]runconvert.TaskDefinition{
	"a":      runconvert.ImportURL("https://example.com/cover.pdf"),
	"b":      runconvert.ImportURL("https://example.com/body.pdf"),
	"merge":  runconvert.ConvertTask("pdf", "pdf", []string{"a", "b"}, nil),
	"export": runconvert.ExportURL("merge"),
}})
job, err = rc.Jobs.Wait(ctx, job.ID, runconvert.WaitOptions{})
for _, f := range job.ExportedFiles() {
	rc.Files.Download(ctx, f.URL, "out", f.Filename)
}

// Upload your own file
job, err = rc.Jobs.Create(ctx, runconvert.JobCreate{Tasks: map[string]runconvert.TaskDefinition{
	"upload":  runconvert.ImportUpload(),
	"convert": runconvert.ConvertTask("png", "webp", "upload", map[string]any{"image_quality": 80}),
	"export":  runconvert.ExportURL("convert"),
}})
err = rc.Files.Upload(ctx, job.Task("upload").UploadURL, "image.png", "")
```

| Method | What it does |
|---|---|
| `rc.Jobs.Create / Get / List / All / Wait / Cancel / Delete` | Jobs, with paging (`List`, `All`) and polling (`Wait`) |
| `rc.Files.Upload(ctx, uploadURL, pathOrReader, name)` / `Download(ctx, url, dir, name)` | Transfers |
| `rc.Operations.List(ctx, category)` / `ForInput(ctx, "mp4")` / `ForPair(ctx, "mp4", "mp3")` | Formats, targets, engines (no key needed) |
| `rc.Me(ctx)` | Account, team and credit balance |

## Webhooks

```go
http.HandleFunc("/webhooks/runconvert", func(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	ev, err := runconvert.ConstructEvent(body, r.Header.Get("X-RunConvert-Signature"), os.Getenv("RUNCONVERT_WEBHOOK_SECRET"))
	if err != nil {
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	if ev.Event == "job.finished" {
		log.Println("done", ev.Job.ID)
	}
})
```

## Errors

| Error | When |
|---|---|
| `*runconvert.Error` | The API said no: `Status`, `Code` (e.g. `BILLING_INSUFFICIENT_CREDITS`), `Message` |
| `*runconvert.JobFailedError` | The job ended in `error`; `Task.Code`, `Task.Reason`, `Task.Message` say why |
| `runconvert.ErrWaitTimeout` (wrapped) | `Wait` ran past its `Timeout` |

Network errors, `429` and `5xx` are retried twice with backoff (`WithMaxRetries`). Creating a job is only
retried when the request never reached the API, so a job is never created twice.

## Development

```bash
go test ./...                                              # no network
RUNCONVERT_API_KEY=rc_live_… go test -run Live -v ./...    # real API, about 2 credits
```

## License

MIT
