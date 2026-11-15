package runconvert

import "encoding/json"

// Job statuses (tasks use the same).
const (
	StatusWaiting    = "waiting"
	StatusProcessing = "processing"
	StatusFinished   = "finished"
	StatusError      = "error"
)

// TaskDefinition is one named task in a job: import/url, import/direct/upload, convert or
// export/url. Build them with ImportURL, ImportUpload, ConvertTask and ExportURL, or as a map.
type TaskDefinition map[string]any

// ImportURL imports a file from a public http(s) URL.
func ImportURL(sourceURL string) TaskDefinition {
	return TaskDefinition{"operation": "import/url", "source_url": sourceURL}
}

// ImportUpload creates an upload task; the created task carries UploadURL (see Files.Upload).
func ImportUpload() TaskDefinition { return TaskDefinition{"operation": "import/direct/upload"} }

// ConvertTask converts the output of input (a task name, or []string to merge/combine).
// options are engine settings from Operations.ForPair, e.g. {"audioBitrate": "192k"}.
func ConvertTask(inputFormat, outputFormat string, input any, options map[string]any) TaskDefinition {
	t := TaskDefinition{}
	for k, v := range options {
		t[k] = v
	}
	t["operation"], t["input"], t["input_format"], t["output_format"] = "convert", input, inputFormat, outputFormat
	return t
}

// ExportURL makes temporary download links for input (a task name or []string).
func ExportURL(input any) TaskDefinition {
	return TaskDefinition{"operation": "export/url", "input": input}
}

// JobCreate is the body of Jobs.Create.
type JobCreate struct {
	Tasks    map[string]TaskDefinition `json:"tasks"`
	Tag      string                    `json:"tag,omitempty"`
	Metadata map[string]any            `json:"metadata,omitempty"`
}

// File is a result file.
type File struct {
	Filename  string `json:"filename"`
	URL       string `json:"url"` // temporary download link
	Size      int64  `json:"size,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// Task is a step of a job. On error, Code is the category and Reason the exact, stable cause.
type Task struct {
	ID             string   `json:"id,omitempty"`
	JobID          string   `json:"job_id,omitempty"`
	Name           string   `json:"name"`
	Operation      string   `json:"operation"`
	Status         string   `json:"status"`
	Percent        int      `json:"percent,omitempty"`
	Message        string   `json:"message,omitempty"`
	Engine         string   `json:"engine,omitempty"`
	EngineVersion  string   `json:"engine_version,omitempty"`
	Credits        int      `json:"credits,omitempty"`
	Input          []string `json:"input,omitempty"`
	UploadURL      string   `json:"upload_url,omitempty"`
	UploadDeadline string   `json:"upload_deadline,omitempty"`
	Result         *struct {
		Files []File `json:"files"`
	} `json:"result,omitempty"`
	Code         string         `json:"code,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	ReasonParams map[string]any `json:"reason_params,omitempty"`
	CreatedAt    string         `json:"created_at,omitempty"`
	StartedAt    *string        `json:"started_at,omitempty"`
	EndedAt      *string        `json:"ended_at,omitempty"`
	ExpiresAt    *string        `json:"expires_at,omitempty"`
}

// Job holds named tasks.
type Job struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	Tag         *string        `json:"tag,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreditsUsed int            `json:"credits_used,omitempty"`
	CreditType  string         `json:"credit_type,omitempty"`
	CreatedAt   string         `json:"created_at,omitempty"`
	StartedAt   *string        `json:"started_at,omitempty"`
	EndedAt     *string        `json:"ended_at,omitempty"`
	ExpiresAt   *string        `json:"expires_at,omitempty"`
	UpdatedAt   string         `json:"updated_at,omitempty"`
	Tasks       []*Task        `json:"tasks,omitempty"`
}

// Task returns the task with this name, or nil.
func (j *Job) Task(name string) *Task {
	for _, t := range j.Tasks {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// ExportedFiles returns the files produced by the job's export/url tasks.
func (j *Job) ExportedFiles() []File {
	var files []File
	for _, t := range j.Tasks {
		if t.Operation == "export/url" && t.Result != nil {
			files = append(files, t.Result.Files...)
		}
	}
	return files
}

// ListMeta is the paging info of a list.
type ListMeta struct {
	Page       int `json:"page"`
	PerPage    int `json:"per_page"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

// Me is the account behind the key.
type Me struct {
	User struct {
		Email    string `json:"email"`
		FullName string `json:"full_name"`
	} `json:"user"`
	Team struct {
		Name string `json:"name"`
	} `json:"team"`
	APIKey *struct {
		Name string `json:"name"`
	} `json:"api_key"`
	Credits *struct {
		Mode           string `json:"mode"`
		DailyLimit     int64  `json:"daily_limit"`
		DailyRemaining int64  `json:"daily_remaining"`
		TotalRemaining int64  `json:"total_remaining"`
	} `json:"credits"`
}

// OutputTarget is a format an input converts to.
type OutputTarget struct {
	Format            string `json:"format"`
	Category          string `json:"category"`
	Engine            string `json:"engine"`
	Credits           int    `json:"credits"`
	BrowserCapable    bool   `json:"browser_capable"`
	BrowserMaxBytes   int64  `json:"browser_max_bytes"`
	BrowserMaxSeconds int    `json:"browser_max_seconds"`
}

// InputFormat is a format and its targets.
type InputFormat struct {
	Format   string         `json:"format"`
	Category string         `json:"category"`
	Outputs  []OutputTarget `json:"outputs"`
}

// OperationsCatalog lists every format.
type OperationsCatalog struct {
	Categories []string      `json:"categories"`
	Formats    []InputFormat `json:"formats"`
}

// EngineOption is a setting a convert task accepts.
type EngineOption struct {
	Name            string            `json:"name"`
	Type            string            `json:"type"`
	Description     string            `json:"description"`
	SupportedValues []json.RawMessage `json:"supported_values"`
}

// Engine converts one pair.
type Engine struct {
	Name    string         `json:"name"`
	Credits int            `json:"credits"`
	Options []EngineOption `json:"options"`
}

// PairEngines are the engines for input → output.
type PairEngines struct {
	Input   string   `json:"input"`
	Output  string   `json:"output"`
	Engines []Engine `json:"engines"`
}

// WebhookEvent is the JSON a webhook delivery carries.
type WebhookEvent struct {
	Event string `json:"event"` // job.finished, task.error, …
	Job   *Job   `json:"job,omitempty"`
	Task  *Task  `json:"task,omitempty"`
}
