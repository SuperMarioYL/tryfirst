package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/SuperMarioYL/tryfirst/internal/plan"
)

// The stub presents deterministic, credential-free OpenAI- and
// Anthropic-compatible endpoints, so an agent-skill demo can authenticate,
// build a request, parse a reply and render without egress or a real key. The
// receipt records model_mode=stub: the evidence is "the wiring runs", never a
// judgement about model quality.
const (
	stubKey      = "tryfirst-stub"
	stubEchoMax  = 120
	stubModel    = "tryfirst-stub"
	statsMarker  = "__TRYFIRST_STUB_STATS__"
	entrypointFn = ".tryfirst-entrypoint.sh"
)

// Stub is the deterministic model server. The zero value is not usable; call
// NewStub.
type Stub struct {
	mu       sync.Mutex
	calls    plan.ModelCalls
	onChange func(plan.ModelCalls)
}

// NewStub returns a stub with zeroed counters.
func NewStub() *Stub { return &Stub{} }

// Stats returns the served request and token counts.
func (s *Stub) Stats() plan.ModelCalls {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *Stub) record(c plan.ModelCalls) {
	s.mu.Lock()
	s.calls = s.calls.Add(c)
	total := s.calls
	onChange := s.onChange
	s.mu.Unlock()
	if onChange != nil {
		onChange(total)
	}
}

// serveStub handles one request. Both the /v1-prefixed and bare paths are
// accepted because SDK base URLs differ in whether they end in /v1.
func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch path {
	case "/v1/models", "/models":
		s.record(plan.ModelCalls{})
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data": []map[string]any{{
				"id":       stubModel,
				"object":   "model",
				"owned_by": "tryfirst",
			}},
		})
	case "/v1/chat/completions", "/chat/completions":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "cannot read request", http.StatusBadRequest)
			return
		}
		req := parseStubRequest(body)
		reply := "tryfirst stub: " + req.echo()
		calls := plan.ModelCalls{Requests: 1, PromptTokens: req.tokens(), CompletionTokens: countTokens(reply)}
		s.record(calls)
		if req.Stream {
			writeChatStream(w, reply)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-tryfirst-stub",
			"object":  "chat.completion",
			"created": 0,
			"model":   stubModel,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": reply},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     calls.PromptTokens,
				"completion_tokens": calls.CompletionTokens,
				"total_tokens":      calls.PromptTokens + calls.CompletionTokens,
			},
		})
	case "/v1/messages", "/messages":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "cannot read request", http.StatusBadRequest)
			return
		}
		req := parseStubRequest(body)
		reply := "tryfirst stub: " + req.echo()
		calls := plan.ModelCalls{Requests: 1, PromptTokens: req.tokens(), CompletionTokens: countTokens(reply)}
		s.record(calls)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":            "msg_tryfirst_stub",
			"type":          "message",
			"role":          "assistant",
			"model":         stubModel,
			"content":       []map[string]any{{"type": "text", "text": reply}},
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  calls.PromptTokens,
				"output_tokens": calls.CompletionTokens,
			},
		})
	default:
		http.Error(w, "tryfirst stub: unknown route "+path, http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "cannot encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeChatStream(w http.ResponseWriter, reply string) {
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	chunk := func(delta map[string]any, finish any) {
		payload, _ := json.Marshal(map[string]any{
			"id":      "chatcmpl-tryfirst-stub",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   stubModel,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
	}
	chunk(map[string]any{"role": "assistant", "content": reply}, nil)
	chunk(map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

type stubMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type stubRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages []stubMessage   `json:"messages"`
	System   json.RawMessage `json:"system"`
}

func parseStubRequest(body []byte) stubRequest {
	var req stubRequest
	_ = json.Unmarshal(body, &req)
	return req
}

// echo mirrors the last user message, so a demo that prints the model reply
// sees its own prompt come back — enough to prove parsing and rendering work.
func (r stubRequest) echo() string {
	var text string
	for _, m := range r.Messages {
		if m.Role == "user" {
			text = rawText(m.Content)
		}
	}
	if text == "" && len(r.Messages) > 0 {
		text = rawText(r.Messages[len(r.Messages)-1].Content)
	}
	text = strings.Join(strings.Fields(text), " ")
	text = truncateRunes(text, stubEchoMax)
	if text == "" {
		return "hello from the tryfirst stub"
	}
	return text
}

func (r stubRequest) tokens() int {
	total := countTokens(rawText(r.System))
	for _, m := range r.Messages {
		total += countTokens(rawText(m.Content))
	}
	if total == 0 {
		total = 1
	}
	return total
}

// rawText flattens a content field that may be a string or an array of blocks.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, block := range blocks {
			if block.Text != "" {
				if b.Len() > 0 {
					b.WriteString(" ")
				}
				b.WriteString(block.Text)
			}
		}
		return b.String()
	}
	return ""
}

func countTokens(s string) int { return len(strings.Fields(s)) }

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// ServeStub runs the stub until ctx is cancelled. The ready file appears once
// the listener is up, so the container entrypoint can wait without tools like
// curl; the stats file is rewritten after every request.
func ServeStub(ctx context.Context, addr, readyFile, statsFile string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("stub listen on %s: %w", addr, err)
	}
	stub := NewStub()
	if statsFile != "" {
		stub.onChange = func(c plan.ModelCalls) { _ = writeStatsFile(statsFile, c) }
		_ = writeStatsFile(statsFile, plan.ModelCalls{})
	}
	if readyFile != "" {
		if err := os.WriteFile(readyFile, []byte("ready\n"), 0o644); err != nil {
			ln.Close()
			return err
		}
	}
	srv := &http.Server{Handler: stub, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
		case <-done:
		}
	}()
	err = srv.Serve(ln)
	close(done)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func writeStatsFile(path string, calls plan.ModelCalls) error {
	data, err := json.Marshal(map[string]int{
		"requests":          calls.Requests,
		"prompt_tokens":     calls.PromptTokens,
		"completion_tokens": calls.CompletionTokens,
	})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ParseStubStats pulls the statistics line the entrypoint prints on stderr and
// returns the remaining stderr for the receipt.
func ParseStubStats(stderr string) (plan.ModelCalls, string) {
	var calls plan.ModelCalls
	kept := make([]string, 0, 8)
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, statsMarker) {
			kept = append(kept, line)
			continue
		}
		var raw struct {
			Requests         int `json:"requests"`
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(trimmed, statsMarker))), &raw); err == nil {
			calls = plan.ModelCalls{Requests: raw.Requests, PromptTokens: raw.PromptTokens, CompletionTokens: raw.CompletionTokens}
		}
	}
	return calls, strings.TrimRight(strings.Join(kept, "\n"), "\n")
}

// StubModelEnv is what the sandbox injects in stub mode. OPENAI_BASE_URL carries
// the /v1 suffix because OpenAI SDKs post the relative path /chat/completions;
// ANTHROPIC_BASE_URL is the bare origin because the Anthropic SDK posts the
// absolute path /v1/messages.
func StubModelEnv() map[string]string {
	base := "http://" + plan.StubAddr
	return map[string]string{
		"OPENAI_BASE_URL":    base + "/v1",
		"OPENAI_API_KEY":     stubKey,
		"OPENAI_MODEL":       stubModel,
		"ANTHROPIC_BASE_URL": base,
		"ANTHROPIC_API_KEY":  stubKey,
		"ANTHROPIC_MODEL":    stubModel,
	}
}

// LiveModelEnv points the container at the host-side gate. The only credential
// the container sees is the per-run ephemeral token.
func LiveModelEnv(hostBase, token string) map[string]string {
	hostBase = strings.TrimSuffix(hostBase, "/")
	return map[string]string{
		"OPENAI_BASE_URL":    hostBase + "/v1",
		"OPENAI_API_KEY":     token,
		"ANTHROPIC_BASE_URL": hostBase,
		"ANTHROPIC_API_KEY":  token,
	}
}

// EntrypointScript is baked into the per-run image. It starts the mounted stub,
// waits for its ready file, runs the demo argv verbatim, forwards the stub's
// statistics on stderr and propagates the demo's exit code.
func EntrypointScript(stubPath, readyFile, statsFile string) string {
	return fmt.Sprintf(`#!/bin/sh
# generated by tryfirst: start the model stub, run the demo, keep its exit code
rm -f "%s" "%s"
"%s" __stub --addr %s --ready "%s" --stats "%s" &
stub=$!
i=0
while [ ! -f "%s" ]; do
  i=$((i+1))
  if [ "$i" -gt 100 ]; then
    echo "tryfirst: model stub did not become ready" >&2
    break
  fi
  sleep 0.1 2>/dev/null || sleep 1
done
"$@"
rc=$?
kill "$stub" 2>/dev/null
if [ -f "%s" ]; then
  printf '\n%s%%s\n' "$(cat "%s")" >&2
fi
exit "$rc"
`, readyFile, statsFile, stubPath, plan.StubAddr, readyFile, statsFile, readyFile, statsFile, statsMarker, statsFile)
}

// BuildLinuxStub produces an arch-matched Linux binary of this program so its
// hidden __stub subcommand can serve model calls inside the container. A
// released binary may ship one and point TRYFIRST_STUB_BIN at it.
func BuildLinuxStub(destDir, goarch string) (string, error) {
	if env := strings.TrimSpace(os.Getenv("TRYFIRST_STUB_BIN")); env != "" {
		if _, err := os.Stat(env); err != nil {
			return "", fmt.Errorf("TRYFIRST_STUB_BIN %s: %w", env, err)
		}
		return env, nil
	}
	src, err := moduleSource()
	if err != nil {
		return "", err
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	dest := filepath.Join(destDir, "tryfirst-stub")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", dest, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cross-build the model stub: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return dest, nil
}

// moduleSource locates this module's source tree so the stub can be built.
func moduleSource() (string, error) {
	if env := strings.TrimSpace(os.Getenv("TRYFIRST_STUB_SRC")); env != "" {
		return env, nil
	}
	if wd, err := os.Getwd(); err == nil {
		for dir := wd; ; dir = filepath.Dir(dir) {
			if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
				strings.Contains(string(data), "module github.com/SuperMarioYL/tryfirst") {
				return dir, nil
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/SuperMarioYL/tryfirst").Output()
	if err != nil {
		return "", fmt.Errorf("locate the module source (set TRYFIRST_STUB_SRC or TRYFIRST_STUB_BIN): %w", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("locate the module source (set TRYFIRST_STUB_SRC or TRYFIRST_STUB_BIN)")
	}
	return dir, nil
}

// EntrypointFilename is the helper script written into the build context.
const EntrypointFilename = entrypointFn

// dockerignore keeps the clone's git metadata out of the image.
const DockerignoreContent = ".git\n"
