// Package plan defines the derived-but-attributable run plan for a repository:
// every install and demo command carries the evidence it came from.
package plan

import (
	"encoding/json"
	"strings"
)

// Verdicts reported for a repository. no_runnable_demo is a result, not an
// error: many repos ship prose instead of a demo.
const (
	VerdictRuns           = "runs"
	VerdictFails          = "fails"
	VerdictNoRunnableDemo = "no_runnable_demo"
	VerdictPlanOnly       = "plan_only"
)

// Model modes recorded on every receipt.
const (
	ModelStub = "stub"
	ModelLive = "live"
)

// Container paths and address of the injected model stub.
const (
	StubAddr           = "127.0.0.1:8787"
	StubMountPath      = "/opt/tryfirst/tryfirst-stub"
	StubEntrypointPath = "/opt/tryfirst/entrypoint.sh"
	StubReadyFile      = "/tmp/tryfirst-stub.ready"
	StubStatsFile      = "/tmp/tryfirst-stub-stats.json"
	StubWorkdir        = "/work"
)

// RunPlan is what detection derives before anything executes. Evidence is the
// provenance of DemoCommand (for example "README:## Quick Start > bash fence
// @L66"); it is empty when NoDemoReason explains why there is nothing to run.
type RunPlan struct {
	Repo         string            `json:"repo"`
	Commit       string            `json:"commit"`
	Runtime      string            `json:"runtime"`
	BaseImage    string            `json:"base_image"`
	Install      []string          `json:"install,omitempty"`
	DemoCommand  []string          `json:"demo_command,omitempty"`
	Evidence     string            `json:"evidence,omitempty"`
	ModelEnv     map[string]string `json:"model_env,omitempty"`
	NoDemoReason string            `json:"no_demo_reason,omitempty"`
}

// Runnable reports whether the plan found something to execute.
func (p RunPlan) Runnable() bool {
	return len(p.DemoCommand) > 0 && p.NoDemoReason == ""
}

// InstallScript joins the detected install commands into one shell line.
func (p RunPlan) InstallScript() string {
	return strings.Join(p.Install, " && ")
}

// DemoScript returns the demo command as a printable shell line.
func (p RunPlan) DemoScript() string {
	if len(p.DemoCommand) == 0 {
		return ""
	}
	if len(p.DemoCommand) == 3 && p.DemoCommand[0] == "sh" && p.DemoCommand[1] == "-lc" {
		return p.DemoCommand[2]
	}
	return strings.Join(p.DemoCommand, " ")
}

// Dockerfile renders the per-run image: the detected base image, the detected
// install commands, and an entrypoint that starts the model stub and then runs
// whatever demo command is passed to the container.
func (p RunPlan) Dockerfile() string {
	var b strings.Builder
	b.WriteString("# rendered from the detected run plan\n")
	b.WriteString("FROM " + p.BaseImage + "\n")
	b.WriteString("ENV PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1\n")
	b.WriteString("WORKDIR " + StubWorkdir + "\n")
	b.WriteString("COPY . " + StubWorkdir + "\n")
	if script := p.InstallScript(); script != "" {
		b.WriteString("RUN " + script + "\n")
	}
	b.WriteString("COPY .tryfirst-entrypoint.sh " + StubEntrypointPath + "\n")
	b.WriteString("RUN chmod 0755 " + StubEntrypointPath + "\n")
	b.WriteString("ENTRYPOINT [\"/bin/sh\", \"" + StubEntrypointPath + "\"]\n")
	return b.String()
}

// ModelCalls is the model-call accounting recorded on a receipt.
type ModelCalls struct {
	Requests         int `json:"requests"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Add accumulates another call count.
func (m ModelCalls) Add(o ModelCalls) ModelCalls {
	return ModelCalls{
		Requests:         m.Requests + o.Requests,
		PromptTokens:     m.PromptTokens + o.PromptTokens,
		CompletionTokens: m.CompletionTokens + o.CompletionTokens,
	}
}

// CanonicalJSON is the canonical byte form used to seal receipts. encoding/json
// emits struct fields in declaration order and map keys in sorted order, so the
// output is stable for these types.
func CanonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}
