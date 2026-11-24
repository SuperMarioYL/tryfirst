// Package gate is the host-side model gateway for live mode. The container
// only ever holds a per-run ephemeral token; the operator's upstream credential
// stays in this process, and the run is cut at a request, token and time cap.
package gate

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/SuperMarioYL/tryfirst/internal/plan"
)

// Environment variables that configure the operator-held upstream. With either
// missing, live mode fails closed before a container starts.
const (
	EnvUpstreamBaseURL = "TRYFIRST_UPSTREAM_BASE_URL"
	EnvUpstreamAPIKey  = "TRYFIRST_UPSTREAM_API_KEY"
)

// Limits are the per-run caps enforced by the gate.
type Limits struct {
	MaxRequests int
	MaxTokens   int
	TimeLimit   time.Duration
}

// DefaultLimits match the documented caps: 20 requests, 50k tokens, 90s.
func DefaultLimits() Limits {
	return Limits{MaxRequests: 20, MaxTokens: 50_000, TimeLimit: 90 * time.Second}
}

// Gate proxies model calls and enforces Limits.
type Gate struct {
	upstream    *url.URL
	upstreamKey string
	limits      Limits
	token       string
	client      *http.Client

	mu               sync.Mutex
	requests         int
	promptTokens     int
	completionTokens int
	deadline         time.Time
	capped           bool
	capHit           bool

	listener net.Listener
	server   *http.Server
}

// FromEnvironment builds a gate from the operator environment. An unset
// upstream is an error so live mode stops before any container exists.
func FromEnvironment() (*Gate, error) {
	base := strings.TrimSpace(os.Getenv(EnvUpstreamBaseURL))
	key := strings.TrimSpace(os.Getenv(EnvUpstreamAPIKey))
	if base == "" || key == "" {
		return nil, fmt.Errorf("live model mode needs %s and %s; no container was started", EnvUpstreamBaseURL, EnvUpstreamAPIKey)
	}
	return New(base, key, DefaultLimits())
}

// New builds a gate pointed at the operator's upstream.
func New(upstreamBaseURL, upstreamKey string, limits Limits) (*Gate, error) {
	parsed, err := url.Parse(strings.TrimSpace(upstreamBaseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid upstream base URL %q", upstreamBaseURL)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	parsed.RawQuery = ""
	if limits.MaxRequests <= 0 {
		limits.MaxRequests = DefaultLimits().MaxRequests
	}
	if limits.MaxTokens <= 0 {
		limits.MaxTokens = DefaultLimits().MaxTokens
	}
	if limits.TimeLimit <= 0 {
		limits.TimeLimit = DefaultLimits().TimeLimit
	}
	token, err := ephemeralToken()
	if err != nil {
		return nil, err
	}
	return &Gate{
		upstream:    parsed,
		upstreamKey: upstreamKey,
		limits:      limits,
		token:       token,
		client:      &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func ephemeralToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "tryfirst-" + hex.EncodeToString(buf), nil
}

// Start binds the gate on an ephemeral loopback port.
func (g *Gate) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("gate listen: %w", err)
	}
	g.listener = ln
	g.deadline = time.Now().Add(g.limits.TimeLimit)
	g.server = &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = g.server.Serve(ln) }()
	return nil
}

// Close stops the gate.
func (g *Gate) Close() {
	if g.server != nil {
		_ = g.server.Close()
	}
}

// Addr is the loopback address the container calls, e.g. 127.0.0.1:53412.
func (g *Gate) Addr() string {
	if g.listener == nil {
		return ""
	}
	return g.listener.Addr().String()
}

// Port is the gate's loopback port.
func (g *Gate) Port() string {
	_, port, err := net.SplitHostPort(g.Addr())
	if err != nil {
		return ""
	}
	return port
}

// Token is the ephemeral credential injected into the container.
func (g *Gate) Token() string { return g.token }

// ModelCalls reports what the gate forwarded.
func (g *Gate) ModelCalls() plan.ModelCalls {
	g.mu.Lock()
	defer g.mu.Unlock()
	return plan.ModelCalls{Requests: g.requests, PromptTokens: g.promptTokens, CompletionTokens: g.completionTokens}
}

// CapHit reports whether a request was refused because a cap was reached.
func (g *Gate) CapHit() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.capHit
}

func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.authorized(r) {
		http.Error(w, "tryfirst gate: missing or invalid run token", http.StatusUnauthorized)
		return
	}
	if !g.admit() {
		http.Error(w, "tryfirst gate: cap reached", http.StatusTooManyRequests)
		return
	}
	g.proxy(w, r)
}

func (g *Gate) authorized(r *http.Request) bool {
	if token := strings.TrimPrefix(r.Header.Get("authorization"), "Bearer "); token == g.token {
		return true
	}
	return r.Header.Get("x-api-key") == g.token
}

func (g *Gate) admit() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.capped || time.Now().After(g.deadline) || g.requests >= g.limits.MaxRequests {
		g.capped = true
		g.capHit = true
		return false
	}
	g.requests++
	return true
}

func (g *Gate) proxy(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "tryfirst gate: cannot read request", http.StatusBadRequest)
		return
	}
	target := *g.upstream
	target.Path = g.upstream.Path + upstreamPath(r.URL.Path)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "tryfirst gate: cannot build upstream request", http.StatusBadGateway)
		return
	}
	req.Header.Set("content-type", "application/json")
	for _, header := range []string{"anthropic-version", "anthropic-beta", "openai-beta", "accept"} {
		if v := r.Header.Get(header); v != "" {
			req.Header.Set(header, v)
		}
	}
	// The container's ephemeral token stops here; the operator credential is
	// attached only on the host side of the gate.
	req.Header.Set("authorization", "Bearer "+g.upstreamKey)
	req.Header.Set("x-api-key", g.upstreamKey)

	resp, err := g.client.Do(req)
	if err != nil {
		http.Error(w, "tryfirst gate: upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		http.Error(w, "tryfirst gate: cannot read upstream response", http.StatusBadGateway)
		return
	}
	if ct := resp.Header.Get("content-type"); ct != "" {
		w.Header().Set("content-type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
	g.addUsage(out)
}

// upstreamPath keeps /v1-prefixed paths and adds the prefix to bare ones, so
// both OpenAI- and Anthropic-style clients reach the same upstream.
func upstreamPath(path string) string {
	if path == "/v1" || strings.HasPrefix(path, "/v1/") {
		return path
	}
	return "/v1" + path
}

func (g *Gate) addUsage(body []byte) {
	var envelope struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return
	}
	prompt := envelope.Usage.PromptTokens + envelope.Usage.InputTokens
	completion := envelope.Usage.CompletionTokens + envelope.Usage.OutputTokens

	g.mu.Lock()
	defer g.mu.Unlock()
	g.promptTokens += prompt
	g.completionTokens += completion
	if g.promptTokens+g.completionTokens >= g.limits.MaxTokens {
		g.capped = true
	}
}

// HostGatewayBase is the address a container reaches the host on.
func HostGatewayBase(port string) string {
	return "http://host.docker.internal:" + port
}
