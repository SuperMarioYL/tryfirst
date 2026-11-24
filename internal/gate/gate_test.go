package gate_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SuperMarioYL/tryfirst/internal/gate"
	"github.com/SuperMarioYL/tryfirst/internal/sandbox"
)

func startGate(t *testing.T, upstream string, limits gate.Limits) *gate.Gate {
	t.Helper()
	g, err := gate.New(upstream, "operator-key", limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g
}

func call(t *testing.T, base, token, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestFromEnvironmentFailsClosed(t *testing.T) {
	cases := []struct {
		name, base, key string
	}{
		{"both unset", "", ""},
		{"only base", "https://upstream.invalid/v1", ""},
		{"only key", "", "sk-test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(gate.EnvUpstreamBaseURL, tc.base)
			t.Setenv(gate.EnvUpstreamAPIKey, tc.key)
			if _, err := gate.FromEnvironment(); err == nil {
				t.Fatal("live mode must fail closed without an upstream")
			}
		})
	}
}

func TestFromEnvironmentAcceptsConfiguredUpstream(t *testing.T) {
	t.Setenv(gate.EnvUpstreamBaseURL, "https://upstream.invalid/v1")
	t.Setenv(gate.EnvUpstreamAPIKey, "sk-operator")
	g, err := gate.FromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if g.Token() == "" || g.Token() == "sk-operator" {
		t.Fatalf("token = %q", g.Token())
	}
	if g.ModelCalls().Requests != 0 {
		t.Fatalf("a gate that has not started must have no calls")
	}
}

func TestEphemeralTokenPerRun(t *testing.T) {
	first := startGate(t, "https://upstream.invalid", gate.DefaultLimits())
	second := startGate(t, "https://upstream.invalid", gate.DefaultLimits())
	if first.Token() == second.Token() {
		t.Fatal("two runs share a token")
	}
	if !strings.HasPrefix(first.Token(), "tryfirst-") {
		t.Fatalf("token = %q", first.Token())
	}
}

func TestGateForwardsThreeCallsToStubUpstream(t *testing.T) {
	stub := sandbox.NewStub()
	upstream := httptest.NewServer(stub)
	defer upstream.Close()
	g := startGate(t, upstream.URL, gate.DefaultLimits())

	for i := 0; i < 3; i++ {
		if status := call(t, g.Addr(), g.Token(), `{"messages":[{"role":"user","content":"ping"}]}`); status != http.StatusOK {
			t.Fatalf("call %d: status %d", i+1, status)
		}
	}
	calls := g.ModelCalls()
	if calls.Requests != 3 {
		t.Fatalf("gate requests = %d", calls.Requests)
	}
	if stubCalls := stub.Stats(); stubCalls.Requests != 3 {
		t.Fatalf("upstream saw %d calls, want 3", stubCalls.Requests)
	}
	if g.CapHit() {
		t.Fatal("cap hit on three calls")
	}
}

func TestGateStopsAtRequestCap(t *testing.T) {
	upstream := httptest.NewServer(sandbox.NewStub())
	defer upstream.Close()
	limits := gate.DefaultLimits()
	g := startGate(t, upstream.URL, limits)

	ok, refused := 0, 0
	for i := 0; i < 100; i++ {
		switch call(t, g.Addr(), g.Token(), `{"messages":[{"role":"user","content":"loop"}]}`) {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			refused++
		default:
			t.Fatalf("unexpected status at call %d", i+1)
		}
	}
	if ok != limits.MaxRequests {
		t.Fatalf("forwarded %d calls, want %d", ok, limits.MaxRequests)
	}
	if refused != 100-limits.MaxRequests {
		t.Fatalf("refused %d calls", refused)
	}
	if !g.CapHit() {
		t.Fatal("cap hit was not recorded")
	}
	if calls := g.ModelCalls(); calls.Requests != limits.MaxRequests {
		t.Fatalf("requests = %d", calls.Requests)
	}
}

func TestGateStopsAtTokenCap(t *testing.T) {
	upstream := httptest.NewServer(sandbox.NewStub())
	defer upstream.Close()
	g := startGate(t, upstream.URL, gate.Limits{MaxRequests: 20, MaxTokens: 1, TimeLimit: time.Minute})

	if status := call(t, g.Addr(), g.Token(), `{"messages":[{"role":"user","content":"tokens"}]}`); status != http.StatusOK {
		t.Fatalf("first call status = %d", status)
	}
	if status := call(t, g.Addr(), g.Token(), `{"messages":[{"role":"user","content":"tokens"}]}`); status != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d, want 429", status)
	}
	if !g.CapHit() {
		t.Fatal("token cap was not recorded")
	}
}

func TestGateStopsAtTimeCap(t *testing.T) {
	upstream := httptest.NewServer(sandbox.NewStub())
	defer upstream.Close()
	g := startGate(t, upstream.URL, gate.Limits{MaxRequests: 20, MaxTokens: 1000, TimeLimit: 20 * time.Millisecond})
	time.Sleep(60 * time.Millisecond)

	if status := call(t, g.Addr(), g.Token(), `{"messages":[{"role":"user","content":"late"}]}`); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if !g.CapHit() {
		t.Fatal("time cap was not recorded")
	}
}

func TestGateRejectsRequestsWithoutTheRunToken(t *testing.T) {
	upstream := httptest.NewServer(sandbox.NewStub())
	defer upstream.Close()
	g := startGate(t, upstream.URL, gate.DefaultLimits())

	if status := call(t, g.Addr(), "", `{"messages":[]}`); status != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", status)
	}
	if status := call(t, g.Addr(), "not-the-token", `{"messages":[]}`); status != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", status)
	}
	if calls := g.ModelCalls(); calls.Requests != 0 {
		t.Fatalf("unauthorized requests were forwarded: %+v", calls)
	}
}

func TestGateHoldsTheOperatorCredential(t *testing.T) {
	seen := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer upstream.Close()
	g := startGate(t, upstream.URL, gate.DefaultLimits())

	if status := call(t, g.Addr(), g.Token(), `{"messages":[]}`); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	headers := <-seen
	if got := headers.Get("authorization"); got != "Bearer operator-key" {
		t.Fatalf("upstream authorization = %q", got)
	}
	if got := headers.Get("x-api-key"); got != "operator-key" {
		t.Fatalf("upstream x-api-key = %q", got)
	}
	if calls := g.ModelCalls(); calls.PromptTokens != 3 || calls.CompletionTokens != 2 {
		t.Fatalf("usage not counted: %+v", calls)
	}
}

func TestGateHandlesUnreachableUpstream(t *testing.T) {
	upstream := httptest.NewServer(sandbox.NewStub())
	url := upstream.URL
	upstream.Close()
	g := startGate(t, url, gate.DefaultLimits())

	if status := call(t, g.Addr(), g.Token(), `{"messages":[]}`); status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
}

func TestGateConcurrentCallsRespectTheCap(t *testing.T) {
	upstream := httptest.NewServer(sandbox.NewStub())
	defer upstream.Close()
	limits := gate.Limits{MaxRequests: 5, MaxTokens: 100000, TimeLimit: time.Minute}
	g := startGate(t, upstream.URL, limits)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, "http://"+g.Addr()+"/v1/chat/completions",
				strings.NewReader(`{"messages":[{"role":"user","content":"x"}]}`))
			if err != nil {
				return
			}
			req.Header.Set("authorization", "Bearer "+g.Token())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	if calls := g.ModelCalls(); calls.Requests != limits.MaxRequests {
		t.Fatalf("requests = %d, want %d", calls.Requests, limits.MaxRequests)
	}
}
