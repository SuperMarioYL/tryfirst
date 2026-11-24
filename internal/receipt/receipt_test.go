package receipt_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SuperMarioYL/tryfirst/internal/plan"
	"github.com/SuperMarioYL/tryfirst/internal/receipt"
)

func sampleReceipt() receipt.Receipt {
	return receipt.Receipt{
		Schema:    receipt.Schema,
		RunID:     "a1b2c3d4",
		StartedAt: time.Date(2026, 11, 24, 21, 0, 0, 0, time.UTC),
		Plan: plan.RunPlan{
			Repo:        "https://github.com/acme/demo",
			Commit:      "abc12345",
			Runtime:     "python-3.12",
			BaseImage:   "python:3.12-slim",
			Install:     []string{"pip install -r requirements.txt"},
			DemoCommand: []string{"sh", "-lc", "python demo.py"},
			Evidence:    "README:Quick start > bash fence @L7",
			ModelEnv:    map[string]string{"OPENAI_BASE_URL": "http://127.0.0.1:8787/v1"},
		},
		ModelMode:  plan.ModelStub,
		Verdict:    plan.VerdictRuns,
		ExitCode:   0,
		DurationMS: 3812,
		Network:    "none",
		Stdout:     "demo ran\n",
		Stderr:     "",
		ModelCalls: plan.ModelCalls{Requests: 2, PromptTokens: 12, CompletionTokens: 9},
	}
}

func TestSealAndVerify(t *testing.T) {
	r := sampleReceipt()
	if err := receipt.Seal(&r); err != nil {
		t.Fatal(err)
	}
	if len(r.Digest) != 64 {
		t.Fatalf("digest = %q", r.Digest)
	}
	if err := receipt.Verify(r); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Sealing twice with unchanged content is stable.
	sealed := r.Digest
	if err := receipt.Seal(&r); err != nil {
		t.Fatal(err)
	}
	if r.Digest != sealed {
		t.Fatalf("seal is not idempotent: %s then %s", sealed, r.Digest)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	r := sampleReceipt()
	if err := receipt.Seal(&r); err != nil {
		t.Fatal(err)
	}
	r.Verdict = plan.VerdictFails
	if err := receipt.Verify(r); err == nil {
		t.Fatal("expected a digest mismatch after tampering")
	}
}

func TestWriteProducesDigestSealedJSONAndReport(t *testing.T) {
	dir := t.TempDir()
	r := sampleReceipt()
	paths, err := receipt.Write(dir, "acme-demo-abc12345", &r)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.JSON, paths.HTML, paths.Log} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if filepath.Base(paths.JSON) != "acme-demo-abc12345.json" {
		t.Fatalf("json name = %s", paths.JSON)
	}

	raw, err := os.ReadFile(paths.JSON)
	if err != nil {
		t.Fatal(err)
	}
	var parsed receipt.Receipt
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("receipt is not JSON: %v", err)
	}
	if parsed.Schema != receipt.Schema {
		t.Fatalf("schema = %q", parsed.Schema)
	}
	if err := receipt.Verify(parsed); err != nil {
		t.Fatalf("written receipt does not verify: %v", err)
	}

	// Recompute the seal independently from the written record.
	canonicalCopy := parsed
	canonicalCopy.Digest = ""
	canonical, err := plan.CanonicalJSON(canonicalCopy)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	if parsed.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest does not recompute: recorded %s", parsed.Digest)
	}

	if len(parsed.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", parsed.Artifacts)
	}
	artifact := parsed.Artifacts[0]
	if artifact.Path != "acme-demo-abc12345.log" {
		t.Fatalf("artifact path = %q", artifact.Path)
	}
	logBytes, err := os.ReadFile(paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	logSum := sha256.Sum256(logBytes)
	if artifact.SHA256 != hex.EncodeToString(logSum[:]) || artifact.Bytes != int64(len(logBytes)) {
		t.Fatalf("artifact hash does not match the log: %+v", artifact)
	}
	if !strings.Contains(string(logBytes), "demo ran") {
		t.Fatalf("log = %s", logBytes)
	}

	html, err := os.ReadFile(paths.HTML)
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, want := range []string{
		"tryfirst/receipt@1",
		plan.VerdictRuns,
		"README:Quick start &gt; bash fence @L7",
		parsed.Digest,
		"python:3.12-slim",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("report missing %q", want)
		}
	}
}

func TestReportIsSelfContained(t *testing.T) {
	r := sampleReceipt()
	html, err := receipt.RenderHTML(r)
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, forbidden := range []string{"<script", "<link", "<img", "src=", "@import", "url("} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("report references an external or active asset (%q)", forbidden)
		}
	}
	if !strings.Contains(page, "Model calls") || !strings.Contains(page, "Plan and provenance") {
		t.Fatal("report lost its sections")
	}
}

func TestReportEscapesCapturedOutput(t *testing.T) {
	r := sampleReceipt()
	r.Stdout = "<script>alert('x')</script> & done"
	html, err := receipt.RenderHTML(r)
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	if strings.Contains(page, "<script>alert") {
		t.Fatal("captured output was not escaped")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatalf("escaped output missing:\n%s", page)
	}
}

func TestTwoRunsOfOneCommitSharePlanAndDifferInIdentity(t *testing.T) {
	first := sampleReceipt()
	first.RunID = "11111111"
	second := sampleReceipt()
	second.RunID = "22222222"
	second.StartedAt = first.StartedAt.Add(90 * time.Second)

	dirA, dirB := t.TempDir(), t.TempDir()
	pathsA, err := receipt.Write(dirA, "acme-demo-abc12345", &first)
	if err != nil {
		t.Fatal(err)
	}
	pathsB, err := receipt.Write(dirB, "acme-demo-abc12345", &second)
	if err != nil {
		t.Fatal(err)
	}
	rawA, _ := os.ReadFile(pathsA.JSON)
	rawB, _ := os.ReadFile(pathsB.JSON)
	var a, b receipt.Receipt
	if err := json.Unmarshal(rawA, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawB, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Plan, b.Plan) {
		t.Fatalf("plan blocks differ across runs of one commit:\n%+v\n%+v", a.Plan, b.Plan)
	}
	if a.RunID == b.RunID || a.StartedAt.Equal(b.StartedAt) {
		t.Fatal("run identity is not unique")
	}
	if a.Digest == b.Digest {
		t.Fatal("digests must differ when run identity differs")
	}
}

func TestWriteTruncatesLargeOutput(t *testing.T) {
	dir := t.TempDir()
	r := sampleReceipt()
	r.Stdout = strings.Repeat("line of output\n", receipt.MaxLogBytes/8)
	paths, err := receipt.Write(dir, "acme-demo-abc12345", &r)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(paths.JSON)
	if err != nil {
		t.Fatal(err)
	}
	var parsed receipt.Receipt
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Stdout) > receipt.MaxLogBytes+200 {
		t.Fatalf("stdout was not truncated: %d bytes", len(parsed.Stdout))
	}
	if !strings.Contains(parsed.Stdout, "truncated") {
		t.Fatal("truncation marker missing")
	}
}

func TestBaseName(t *testing.T) {
	cases := []struct {
		owner, repo, commit, want string
	}{
		{"SuperMarioYL", "tryfirst", "abcdef1234567890", "supermarioyl-tryfirst-abcdef12"},
		{"acme", "demo", "abc123", "acme-demo-abc123"},
		{"acme", "my repo", "zzz", "acme-my-repo-zzz"},
		{"local", "My.Dir", "nogit", "local-my.dir-nogit"},
	}
	for _, tc := range cases {
		if got := receipt.BaseName(tc.owner, tc.repo, tc.commit); got != tc.want {
			t.Fatalf("BaseName(%q,%q,%q) = %q, want %q", tc.owner, tc.repo, tc.commit, got, tc.want)
		}
	}
}

func TestNoDemoReceipt(t *testing.T) {
	dir := t.TempDir()
	r := sampleReceipt()
	r.Verdict = plan.VerdictNoRunnableDemo
	r.ExitCode = -1
	r.Plan.DemoCommand = nil
	r.Plan.Evidence = ""
	r.Plan.NoDemoReason = `README "Quick Start" has no fenced code block`
	r.Stdout = ""
	r.ModelCalls = plan.ModelCalls{}
	paths, err := receipt.Write(dir, "acme-demo-abc12345", &r)
	if err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(paths.HTML)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "no_runnable_demo") {
		t.Fatal("verdict missing from report")
	}
	if !strings.Contains(string(page), "no fenced code block") {
		t.Fatal("reason missing from report")
	}
	if !strings.Contains(string(page), "none detected") {
		t.Fatal("report should say no demo was detected")
	}
}
