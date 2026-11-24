package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SuperMarioYL/tryfirst/internal/plan"
)

func postJSON(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, out
}

func TestStubChatCompletions(t *testing.T) {
	stub := NewStub()
	server := httptest.NewServer(stub)
	defer server.Close()

	status, body := postJSON(t, server.URL+"/v1/chat/completions",
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello stub"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}
	var got struct {
		Object  string `json:"object"`
		Created int    `json:"created"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, body)
	}
	if got.Object != "chat.completion" || len(got.Choices) != 1 {
		t.Fatalf("unexpected envelope: %s", body)
	}
	if got.Choices[0].Message.Content != "tryfirst stub: hello stub" {
		t.Fatalf("content = %q", got.Choices[0].Message.Content)
	}
	if got.Choices[0].Message.Role != "assistant" || got.Choices[0].FinishReason != "stop" {
		t.Fatalf("choice = %+v", got.Choices[0])
	}
	if got.Created != 0 {
		t.Fatalf("created must be deterministic, got %d", got.Created)
	}
	if got.Usage.PromptTokens == 0 || got.Usage.TotalTokens == 0 {
		t.Fatalf("usage = %+v", got.Usage)
	}
	if calls := stub.Stats(); calls.Requests != 1 {
		t.Fatalf("stats = %+v", calls)
	}
}

func TestStubChatStreaming(t *testing.T) {
	server := httptest.NewServer(NewStub())
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"stream me"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, "text/event-stream") && resp.Header.Get("content-type") != "text/event-stream" {
		t.Fatalf("content-type = %q", resp.Header.Get("content-type"))
	}
	for _, want := range []string{"chat.completion.chunk", "tryfirst stub: stream me", "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q:\n%s", want, text)
		}
	}
}

func TestStubAnthropicMessages(t *testing.T) {
	stub := NewStub()
	server := httptest.NewServer(stub)
	defer server.Close()

	status, body := postJSON(t, server.URL+"/v1/messages",
		`{"model":"demo-anthropic-model","messages":[{"role":"user","content":[{"type":"text","text":"hi there"}]}]}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}
	var got struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got.Type != "message" || got.Role != "assistant" || got.StopReason != "end_turn" {
		t.Fatalf("envelope = %s", body)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "tryfirst stub: hi there" {
		t.Fatalf("content = %+v", got.Content)
	}
	if got.Usage.InputTokens != 2 {
		t.Fatalf("input_tokens = %d", got.Usage.InputTokens)
	}
	if calls := stub.Stats(); calls.Requests != 1 || calls.CompletionTokens == 0 {
		t.Fatalf("stats = %+v", calls)
	}
}

func TestStubBareAndModelsRoutes(t *testing.T) {
	server := httptest.NewServer(NewStub())
	defer server.Close()

	if status, _ := postJSON(t, server.URL+"/chat/completions", `{"messages":[{"role":"user","content":"bare"}]}`); status != http.StatusOK {
		t.Fatalf("bare chat status = %d", status)
	}
	resp, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "tryfirst-stub") {
		t.Fatalf("models body = %s", body)
	}
}

func TestStubUnknownRoute(t *testing.T) {
	server := httptest.NewServer(NewStub())
	defer server.Close()
	resp, err := http.Get(server.URL + "/v1/embeddings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestStubIsDeterministic(t *testing.T) {
	server := httptest.NewServer(NewStub())
	defer server.Close()
	const body = `{"messages":[{"role":"user","content":"same request"}]}`
	_, first := postJSON(t, server.URL+"/v1/chat/completions", body)
	_, second := postJSON(t, server.URL+"/v1/chat/completions", body)
	if string(first) != string(second) {
		t.Fatalf("stub output differs:\n%s\n%s", first, second)
	}
}

func TestStubStatsAccumulate(t *testing.T) {
	stub := NewStub()
	server := httptest.NewServer(stub)
	defer server.Close()
	for i := 0; i < 3; i++ {
		if status, body := postJSON(t, server.URL+"/v1/chat/completions", `{"messages":[{"role":"user","content":"one two"}]}`); status != http.StatusOK {
			t.Fatalf("status = %d %s", status, body)
		}
	}
	calls := stub.Stats()
	if calls.Requests != 3 {
		t.Fatalf("requests = %d", calls.Requests)
	}
	if calls.PromptTokens != 6 {
		t.Fatalf("prompt tokens = %d, want 6", calls.PromptTokens)
	}
}

func TestStubModelEnv(t *testing.T) {
	env := StubModelEnv()
	if env["OPENAI_BASE_URL"] != "http://127.0.0.1:8787/v1" {
		t.Fatalf("OPENAI_BASE_URL = %q", env["OPENAI_BASE_URL"])
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8787" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q", env["ANTHROPIC_BASE_URL"])
	}
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		if env[key] == "" {
			t.Fatalf("%s is empty", key)
		}
	}
}

func TestLiveModelEnv(t *testing.T) {
	env := LiveModelEnv("http://host.docker.internal:41234/", "tryfirst-token")
	if env["OPENAI_BASE_URL"] != "http://host.docker.internal:41234/v1" {
		t.Fatalf("OPENAI_BASE_URL = %q", env["OPENAI_BASE_URL"])
	}
	if env["ANTHROPIC_BASE_URL"] != "http://host.docker.internal:41234" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q", env["ANTHROPIC_BASE_URL"])
	}
	if env["OPENAI_API_KEY"] != "tryfirst-token" || env["ANTHROPIC_API_KEY"] != "tryfirst-token" {
		t.Fatalf("live mode must inject only the ephemeral token: %v", env)
	}
}

func TestParseStubStats(t *testing.T) {
	stderr := "demo said this\n" +
		"__TRYFIRST_STUB_STATS__{\"requests\":2,\"prompt_tokens\":7,\"completion_tokens\":4}\n" +
		"and this"
	calls, rest := ParseStubStats(stderr)
	if calls != (plan.ModelCalls{Requests: 2, PromptTokens: 7, CompletionTokens: 4}) {
		t.Fatalf("calls = %+v", calls)
	}
	if strings.Contains(rest, "__TRYFIRST_STUB_STATS__") {
		t.Fatalf("marker leaked into captured stderr: %q", rest)
	}
	if !strings.Contains(rest, "demo said this") || !strings.Contains(rest, "and this") {
		t.Fatalf("stderr lost lines: %q", rest)
	}
}

func TestEntrypointScript(t *testing.T) {
	script := EntrypointScript(plan.StubMountPath, plan.StubReadyFile, plan.StubStatsFile)
	for _, want := range []string{
		`"/opt/tryfirst/tryfirst-stub" __stub --addr 127.0.0.1:8787`,
		`while [ ! -f "/tmp/tryfirst-stub.ready" ]`,
		`"$@"`,
		`exit "$rc"`,
		`__TRYFIRST_STUB_STATS__`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("entrypoint missing %q:\n%s", want, script)
		}
	}
}

func TestRenderDockerfile(t *testing.T) {
	p := plan.RunPlan{
		BaseImage:   "python:3.12-slim",
		Install:     []string{"pip install -r requirements.txt", "pip install ."},
		DemoCommand: []string{"python", "demo.py"},
	}
	dockerfile := p.Dockerfile()
	for _, want := range []string{
		"FROM python:3.12-slim",
		"RUN pip install -r requirements.txt && pip install .",
		"COPY .tryfirst-entrypoint.sh /opt/tryfirst/entrypoint.sh",
		`ENTRYPOINT ["/bin/sh", "/opt/tryfirst/entrypoint.sh"]`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("dockerfile missing %q:\n%s", want, dockerfile)
		}
	}
}

func TestDockerRunArgs(t *testing.T) {
	runner := &DockerRunner{Binary: "docker"}
	spec := RunSpec{
		Name:    "tryfirst-abcd1234",
		Image:   "tryfirst-run:abc12345-abcd1234",
		Network: "none",
		Env:     map[string]string{"B_KEY": "2", "A_KEY": "1"},
		Mounts:  []Mount{{HostPath: "/tmp/stub", ContainerPath: "/opt/tryfirst/tryfirst-stub", ReadOnly: true}},
		Command: []string{"sh", "-lc", "python demo.py"},
		Labels:  map[string]string{ManagedLabel: "true"},
	}
	args := runner.runArgs(spec)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run --rm --name tryfirst-abcd1234 --network none",
		"--label tryfirst.managed=true",
		"-e A_KEY=1",
		"-e B_KEY=2",
		"-v /tmp/stub:/opt/tryfirst/tryfirst-stub:ro",
		"-w /work tryfirst-run:abc12345-abcd1234 sh -lc",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("run args missing %q:\n%s", want, joined)
		}
	}
	// Argument order must be deterministic so two runs of one commit are
	// comparable.
	if strings.Index(joined, "A_KEY=1") > strings.Index(joined, "B_KEY=2") {
		t.Fatalf("env vars are not sorted: %s", joined)
	}
	if strings.Contains(joined, "--add-host") {
		t.Fatalf("no-network runs must not map the host gateway: %s", joined)
	}
}

func TestDockerRunArgsLiveModeMapsHostGateway(t *testing.T) {
	runner := &DockerRunner{Binary: "docker"}
	args := runner.runArgs(RunSpec{
		Name:    "tryfirst-abcd1234",
		Image:   "tryfirst-run:abc12345-abcd1234",
		Network: "bridge",
		Command: []string{"python", "demo.py"},
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--network bridge") || !strings.Contains(joined, "--add-host host.docker.internal:host-gateway") {
		t.Fatalf("live mode args = %s", joined)
	}
}

func TestDockerBuildArgs(t *testing.T) {
	runner := &DockerRunner{Binary: "docker"}
	spec := BuildSpec{
		Ref:            "tryfirst-run:abc12345-abcd1234",
		ContextDir:     "/tmp/repo",
		DockerfilePath: "/tmp/repo/.tryfirst.Dockerfile",
		Labels:         map[string]string{ManagedLabel: "true", "tryfirst.run": "abcd1234"},
	}
	args := runner.buildArgs(spec)
	if args[len(args)-1] != "/tmp/repo" {
		t.Fatalf("context must be last: %v", args)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"build -t tryfirst-run:abc12345-abcd1234 -f /tmp/repo/.tryfirst.Dockerfile",
		"--label tryfirst.managed=true",
		"--label tryfirst.run=abcd1234",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("build args missing %q:\n%s", want, joined)
		}
	}
}

func TestPruneArgs(t *testing.T) {
	args := pruneArgs(ManagedLabel, 7*24*time.Hour)
	joined := strings.Join(args, " ")
	for _, want := range []string{"image prune --all --force", "--filter label=tryfirst.managed", "--filter until=168h"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("prune args missing %q: %s", want, joined)
		}
	}
}

func TestPrepareBuildContext(t *testing.T) {
	dir := t.TempDir()
	p := plan.RunPlan{
		BaseImage:   "node:22-slim",
		Install:     []string{"npm install"},
		DemoCommand: []string{"npm", "run", "demo"},
	}
	if err := PrepareBuildContext(dir, p); err != nil {
		t.Fatal(err)
	}
	dockerfile, err := os.ReadFile(filepath.Join(dir, DockerfileFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "FROM node:22-slim") {
		t.Fatalf("dockerfile = %s", dockerfile)
	}
	entrypoint, err := os.ReadFile(filepath.Join(dir, EntrypointFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entrypoint), "__stub") {
		t.Fatalf("entrypoint = %s", entrypoint)
	}
	ignore, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ignore), ".git") {
		t.Fatalf(".dockerignore = %s", ignore)
	}
}

func TestImageRef(t *testing.T) {
	ref := ImageRef("ABC123456789", "ab12cd34")
	if ref != "tryfirst-run:abc12345-ab12cd34" {
		t.Fatalf("ref = %q", ref)
	}
	if ref := ImageRef("no:git", "ffffff"); ref != "tryfirst-run:no-git-ffffff" {
		t.Fatalf("commit characters must be sanitised, got %q", ref)
	}
}

// fakeRunner is the test double for the docker interface. No test shells out.
type fakeRunner struct {
	mu       sync.Mutex
	pings    int
	builds   []BuildSpec
	runs     []RunSpec
	removals []string
	buildErr error
	runErr   error
	result   RunResult
}

var _ Runner = (*fakeRunner)(nil)

func (f *fakeRunner) Ping(context.Context) error { f.pings++; return nil }

func (f *fakeRunner) ServerArch(context.Context) (string, error) { return "arm64", nil }

func (f *fakeRunner) BuildImage(_ context.Context, spec BuildSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builds = append(f.builds, spec)
	return f.buildErr
}

func (f *fakeRunner) RunContainer(_ context.Context, spec RunSpec) (RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, spec)
	if f.runErr != nil {
		return f.result, f.runErr
	}
	return f.result, nil
}

func (f *fakeRunner) RemoveImage(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removals = append(f.removals, ref)
	return nil
}

func (f *fakeRunner) PruneImages(context.Context, string, time.Duration) (string, error) {
	return "Deleted Images:\nuntagged: x", nil
}

func runnablePlan() plan.RunPlan {
	return plan.RunPlan{
		Repo:        "https://example.invalid/demo",
		Commit:      "abc12345",
		Runtime:     "python-3.12",
		BaseImage:   "python:3.12-slim",
		Install:     []string{"pip install ."},
		DemoCommand: []string{"python", "demo.py"},
		Evidence:    "README:Quick start > bash fence @L7",
	}
}

func TestSessionBuildsRunsAndRemovesImage(t *testing.T) {
	fake := &fakeRunner{result: RunResult{ExitCode: 0, Stdout: "hi\n", DurationMS: 12}}
	work := t.TempDir()
	detected := runnablePlan()
	session := Session{
		Runner:   fake,
		Plan:     detected,
		WorkDir:  work,
		ImageRef: ImageRef(detected.Commit, "ab12cd34"),
		ModelEnv: StubModelEnv(),
		Network:  "none",
		Timeout:  time.Minute,
		RunID:    "ab12cd34",
		StubPath: "/tmp/tryfirst-stub",
	}
	result, err := session.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "hi\n" {
		t.Fatalf("result = %+v", result)
	}
	if len(fake.builds) != 1 {
		t.Fatalf("builds = %d", len(fake.builds))
	}
	if fake.builds[0].Labels[ManagedLabel] != "true" {
		t.Fatalf("image is not labelled for gc: %v", fake.builds[0].Labels)
	}
	if len(fake.runs) != 1 {
		t.Fatalf("runs = %d", len(fake.runs))
	}
	run := fake.runs[0]
	if run.Network != "none" {
		t.Fatalf("network = %q", run.Network)
	}
	if got := strings.Join(run.Command, " "); got != "python demo.py" {
		t.Fatalf("command = %q", got)
	}
	if len(run.Mounts) != 1 || !run.Mounts[0].ReadOnly || run.Mounts[0].ContainerPath != plan.StubMountPath {
		t.Fatalf("mounts = %+v", run.Mounts)
	}
	if run.Env["OPENAI_BASE_URL"] != "http://127.0.0.1:8787/v1" {
		t.Fatalf("env = %v", run.Env)
	}
	if len(fake.removals) != 1 || fake.removals[0] != session.ImageRef {
		t.Fatalf("image was not cleaned up: %v", fake.removals)
	}
}

func TestSessionRemovesImageOnBuildFailure(t *testing.T) {
	fake := &fakeRunner{buildErr: errors.New("build failed")}
	work := t.TempDir()
	session := Session{
		Runner:   fake,
		Plan:     runnablePlan(),
		WorkDir:  work,
		ImageRef: "tryfirst-run:deadbeef",
		Network:  "none",
		RunID:    "deadbeef",
		StubPath: "/tmp/tryfirst-stub",
	}
	if _, err := session.Execute(context.Background()); err == nil {
		t.Fatal("expected the build error")
	}
	if len(fake.removals) != 1 || fake.removals[0] != "tryfirst-run:deadbeef" {
		t.Fatalf("image was not cleaned up after a failed build: %v", fake.removals)
	}
	if len(fake.runs) != 0 {
		t.Fatalf("no container may start after a failed build: %+v", fake.runs)
	}
}

func TestSessionRemovesImageOnRunFailure(t *testing.T) {
	fake := &fakeRunner{runErr: errors.New("docker run failed")}
	session := Session{
		Runner:   fake,
		Plan:     runnablePlan(),
		WorkDir:  t.TempDir(),
		ImageRef: "tryfirst-run:cafebabe",
		Network:  "none",
		RunID:    "cafebabe",
		StubPath: "/tmp/tryfirst-stub",
	}
	if _, err := session.Execute(context.Background()); err == nil {
		t.Fatal("expected the run error")
	}
	if len(fake.removals) != 1 {
		t.Fatalf("image was not cleaned up after a failed run: %v", fake.removals)
	}
}
