// Package receipt seals a run into the tryfirst/receipt@1 contract: what was
// detected, what ran, what came back, and a digest over the canonical JSON.
package receipt

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SuperMarioYL/tryfirst/internal/plan"
)

// Schema is the receipt contract this package writes.
const Schema = "tryfirst/receipt@1"

// MaxLogBytes caps each captured stream stored in a receipt and its log file.
const MaxLogBytes = 64 << 10

// Artifact is one file the run left on the host, hashed for later checking.
type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Receipt answers "does it run" without homework: the plan and its provenance,
// the verdict, the captured output, the model-call count and the seal.
type Receipt struct {
	Schema     string          `json:"schema"`
	RunID      string          `json:"run_id"`
	StartedAt  time.Time       `json:"started_at"`
	Plan       plan.RunPlan    `json:"plan"`
	ModelMode  string          `json:"model_mode"`
	Verdict    string          `json:"verdict"`
	ExitCode   int             `json:"exit_code"`
	DurationMS int64           `json:"duration_ms"`
	Network    string          `json:"network"`
	Stdout     string          `json:"stdout"`
	Stderr     string          `json:"stderr"`
	Artifacts  []Artifact      `json:"artifacts"`
	ModelCalls plan.ModelCalls `json:"model_calls"`
	CapHit     bool            `json:"cap_hit,omitempty"`
	Digest     string          `json:"digest"`
}

// Paths are the files a written receipt produced.
type Paths struct {
	JSON string
	HTML string
	Log  string
}

// BaseName is the receipt file stem: owner-repo-sha8.
func BaseName(owner, repo, commit string) string {
	sha := commit
	if len(sha) > 8 {
		sha = sha[:8]
	}
	name := sanitize(owner) + "-" + sanitize(repo) + "-" + sanitize(sha)
	return strings.Trim(name, "-")
}

func sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// Canonical returns the sealed byte form: compact JSON with Digest emptied.
func (r Receipt) Canonical() ([]byte, error) {
	copy := r
	copy.Digest = ""
	return plan.CanonicalJSON(copy)
}

// Seal writes the sha256 digest over the canonical JSON.
func Seal(r *Receipt) error {
	canonical, err := r.Canonical()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	r.Digest = hex.EncodeToString(sum[:])
	return nil
}

// Verify recomputes the digest and reports a mismatch.
func Verify(r Receipt) error {
	got := r.Digest
	if err := Seal(&r); err != nil {
		return err
	}
	if r.Digest != got {
		return fmt.Errorf("receipt digest mismatch: recorded %s, recomputed %s", got, r.Digest)
	}
	return nil
}

// Write persists the log artifact, seals the receipt, then writes the JSON and
// the self-contained HTML report. Mutation is limited to Artifacts and Digest.
func Write(dir, base string, r *Receipt) (Paths, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Paths{}, err
	}
	paths := Paths{
		JSON: filepath.Join(dir, base+".json"),
		HTML: filepath.Join(dir, base+".html"),
		Log:  filepath.Join(dir, base+".log"),
	}

	r.Stdout = truncateLog(r.Stdout)
	r.Stderr = truncateLog(r.Stderr)
	logBody := combinedLog(r.Stdout, r.Stderr, r.ModelMode)
	if err := os.WriteFile(paths.Log, []byte(logBody), 0o644); err != nil {
		return Paths{}, err
	}
	sum := sha256.Sum256([]byte(logBody))
	r.Artifacts = []Artifact{{
		Path:   filepath.Base(paths.Log),
		SHA256: hex.EncodeToString(sum[:]),
		Bytes:  int64(len(logBody)),
	}}

	if err := Seal(r); err != nil {
		return Paths{}, err
	}
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return Paths{}, err
	}
	if err := os.WriteFile(paths.JSON, append(encoded, '\n'), 0o644); err != nil {
		return Paths{}, err
	}
	html, err := RenderHTML(*r)
	if err != nil {
		return Paths{}, err
	}
	if err := os.WriteFile(paths.HTML, html, 0o644); err != nil {
		return Paths{}, err
	}
	return paths, nil
}

func combinedLog(stdout, stderr, modelMode string) string {
	var b strings.Builder
	b.WriteString("$ tryfirst run (model: " + modelMode + ")\n")
	b.WriteString("── stdout ──\n")
	b.WriteString(stdout)
	if !strings.HasSuffix(stdout, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("── stderr ──\n")
	b.WriteString(stderr)
	if !strings.HasSuffix(stderr, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// truncateLog keeps the head of a stream and marks what was cut.
func truncateLog(s string) string {
	if len(s) <= MaxLogBytes {
		return s
	}
	head := s[:MaxLogBytes]
	if idx := strings.LastIndexByte(head, '\n'); idx > MaxLogBytes/2 {
		head = head[:idx]
	}
	head = strings.ToValidUTF8(head, "")
	return head + fmt.Sprintf("\n...[truncated %d bytes]\n", len(s)-len(head))
}

//go:embed report.html.tmpl
var reportTemplate string

var reportTmpl = template.Must(template.New("report").Parse(reportTemplate))

type reportData struct {
	Receipt      Receipt
	VerdictClass string
	ModeLabel    string
	NetworkLabel string
}

// RenderHTML renders the offline report. No external asset is referenced, so
// the file opens without a network.
func RenderHTML(r Receipt) ([]byte, error) {
	data := reportData{
		Receipt:      r,
		VerdictClass: verdictClass(r.Verdict),
		ModeLabel:    r.ModelMode + " model",
		NetworkLabel: r.Network,
	}
	var b strings.Builder
	if err := reportTmpl.Execute(&b, data); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func verdictClass(verdict string) string {
	switch verdict {
	case plan.VerdictRuns:
		return "ok"
	case plan.VerdictFails:
		return "bad"
	case plan.VerdictNoRunnableDemo:
		return "warn"
	}
	return "info"
}
