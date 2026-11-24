package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/SuperMarioYL/tryfirst/internal/plan"
)

// ManagedLabel marks every image and container Tryfirst creates, so gc can
// prune exactly its own residue.
const ManagedLabel = "tryfirst.managed"

// Mount is a bind mount from the host into the container.
type Mount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// BuildSpec describes one per-run image build.
type BuildSpec struct {
	Ref            string
	ContextDir     string
	DockerfilePath string
	Labels         map[string]string
}

// RunSpec describes one container run.
type RunSpec struct {
	Name    string
	Image   string
	Network string
	Env     map[string]string
	Mounts  []Mount
	Command []string
	Timeout time.Duration
	Labels  map[string]string
}

// RunResult is what a run left behind: exit code, captured streams, timing.
type RunResult struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	DurationMS int64
	TimedOut   bool
}

// Runner is every docker interaction the CLI needs. Tests use a fake, so no
// test process ever talks to a docker daemon.
type Runner interface {
	Ping(ctx context.Context) error
	ServerArch(ctx context.Context) (string, error)
	BuildImage(ctx context.Context, spec BuildSpec) error
	RunContainer(ctx context.Context, spec RunSpec) (RunResult, error)
	RemoveImage(ctx context.Context, ref string) error
	PruneImages(ctx context.Context, label string, olderThan time.Duration) (string, error)
}

// DockerfileFilename is the rendered Dockerfile inside the build context.
const DockerfileFilename = ".tryfirst.Dockerfile"

// PrepareBuildContext writes the rendered Dockerfile, the entrypoint script and
// a minimal .dockerignore into the (already temporary) checkout.
func PrepareBuildContext(dir string, p plan.RunPlan) error {
	entrypoint := EntrypointScript(plan.StubMountPath, plan.StubReadyFile, plan.StubStatsFile)
	files := map[string]string{
		DockerfileFilename: p.Dockerfile(),
		EntrypointFilename: entrypoint,
		".dockerignore":    DockerignoreContent,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// Session runs one demo in a per-run image and removes the image on every exit
// path. The container is removed by --rm, so teardown leaves no container, no
// image and no temp dir behind.
type Session struct {
	Runner   Runner
	Plan     plan.RunPlan
	WorkDir  string
	ImageRef string
	ModelEnv map[string]string
	Network  string
	Timeout  time.Duration
	RunID    string
	StubPath string
}

// Execute builds the image, runs the demo, and always removes the image.
func (s Session) Execute(ctx context.Context) (RunResult, error) {
	labels := map[string]string{ManagedLabel: "true", "tryfirst.run": s.RunID}
	defer func() { _ = s.Runner.RemoveImage(context.Background(), s.ImageRef) }()

	if err := PrepareBuildContext(s.WorkDir, s.Plan); err != nil {
		return RunResult{}, err
	}
	if err := s.Runner.BuildImage(ctx, BuildSpec{
		Ref:            s.ImageRef,
		ContextDir:     s.WorkDir,
		DockerfilePath: filepath.Join(s.WorkDir, DockerfileFilename),
		Labels:         labels,
	}); err != nil {
		return RunResult{}, err
	}
	return s.Runner.RunContainer(ctx, RunSpec{
		Name:    "tryfirst-" + s.RunID,
		Image:   s.ImageRef,
		Network: s.Network,
		Env:     s.ModelEnv,
		Mounts: []Mount{{
			HostPath:      s.StubPath,
			ContainerPath: plan.StubMountPath,
			ReadOnly:      true,
		}},
		Command: s.Plan.DemoCommand,
		Timeout: s.Timeout,
		Labels:  labels,
	})
}

// ImageRef is the per-run image tag: tryfirst-run:<sha8>-<run id>.
func ImageRef(commit, runID string) string {
	sha := commit
	if len(sha) > 8 {
		sha = sha[:8]
	}
	tag := strings.ToLower(sha + "-" + runID)
	var b strings.Builder
	for _, r := range tag {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return "tryfirst-run:" + strings.Trim(b.String(), "-")
}

// DockerRunner shells out to the docker CLI. All argument assembly is pure and
// tested; only the exec calls need a daemon.
type DockerRunner struct {
	Binary string
}

// NewDocker returns a runner that uses the docker CLI on PATH.
func NewDocker() *DockerRunner { return &DockerRunner{Binary: "docker"} }

func (d *DockerRunner) bin() string {
	if d.Binary != "" {
		return d.Binary
	}
	return "docker"
}

// Ping fails when the daemon is unreachable, before any image work starts.
func (d *DockerRunner) Ping(ctx context.Context) error {
	out, err := d.output(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return fmt.Errorf("cannot reach the Docker daemon (%v); start Docker and retry", err)
	}
	if strings.TrimSpace(out) == "" {
		return errors.New("cannot reach the Docker daemon; start Docker and retry")
	}
	return nil
}

// ServerArch reports the daemon's architecture so the stub binary is built to
// match. It falls back to the host architecture if the query fails.
func (d *DockerRunner) ServerArch(ctx context.Context) (string, error) {
	out, err := d.output(ctx, "version", "--format", "{{.Server.Arch}}")
	if err != nil || strings.TrimSpace(out) == "" {
		return runtime.GOARCH, nil
	}
	switch strings.TrimSpace(out) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return runtime.GOARCH, nil
}

func (d *DockerRunner) output(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, d.bin(), args...).Output()
	return string(out), err
}

// BuildImage builds the per-run image from the rendered Dockerfile.
func (d *DockerRunner) BuildImage(ctx context.Context, spec BuildSpec) error {
	cmd := exec.CommandContext(ctx, d.bin(), d.buildArgs(spec)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build %s: %v: %s", spec.Ref, err, lastLines(stderr.String(), 12))
	}
	return nil
}

func (d *DockerRunner) buildArgs(spec BuildSpec) []string {
	args := []string{"build", "-t", spec.Ref, "-f", spec.DockerfilePath}
	for _, kv := range labelArgs(spec.Labels) {
		args = append(args, "--label", kv)
	}
	return append(args, spec.ContextDir)
}

// RunContainer runs the demo with --rm, so the container is gone when the
// command returns. A timeout kills the container and reports exit code 124.
func (d *DockerRunner) RunContainer(ctx context.Context, spec RunSpec) (RunResult, error) {
	runCtx := ctx
	var cancel context.CancelFunc
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, d.bin(), d.runArgs(spec)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	result := RunResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err == nil {
		return result, nil
	}
	if runCtx.Err() == context.DeadlineExceeded {
		// Killing the CLI may leave the container running, so stop it by name.
		_ = exec.Command(d.bin(), "kill", spec.Name).Run()
		result.ExitCode = 124
		result.TimedOut = true
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

func (d *DockerRunner) runArgs(spec RunSpec) []string {
	args := []string{"run", "--rm", "--name", spec.Name, "--network", spec.Network}
	if spec.Network != "none" {
		// Live mode reaches the host-side gate at host.docker.internal, which
		// Docker Desktop resolves and Linux needs mapped to the gateway.
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	for _, kv := range labelArgs(spec.Labels) {
		args = append(args, "--label", kv)
	}
	for _, key := range sortedKeys(spec.Env) {
		args = append(args, "-e", key+"="+spec.Env[key])
	}
	for _, mount := range spec.Mounts {
		volume := mount.HostPath + ":" + mount.ContainerPath
		if mount.ReadOnly {
			volume += ":ro"
		}
		args = append(args, "-v", volume)
	}
	args = append(args, "-w", "/work", spec.Image)
	return append(args, spec.Command...)
}

// RemoveImage drops the per-run image. A missing image is not an error.
func (d *DockerRunner) RemoveImage(ctx context.Context, ref string) error {
	out, err := exec.CommandContext(ctx, d.bin(), "rmi", "-f", ref).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such image") {
		return fmt.Errorf("docker rmi %s: %v", ref, err)
	}
	return nil
}

// PruneImages removes Tryfirst-labelled images older than olderThan.
func (d *DockerRunner) PruneImages(ctx context.Context, label string, olderThan time.Duration) (string, error) {
	out, err := exec.CommandContext(ctx, d.bin(), pruneArgs(label, olderThan)...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker image prune: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func pruneArgs(label string, olderThan time.Duration) []string {
	hours := int(olderThan.Hours())
	if hours < 1 {
		hours = 1
	}
	return []string{
		"image", "prune", "--all", "--force",
		"--filter", "label=" + label,
		"--filter", fmt.Sprintf("until=%dh", hours),
	}
}

func labelArgs(labels map[string]string) []string {
	out := make([]string, 0, len(labels))
	for _, key := range sortedKeys(labels) {
		out = append(out, key+"="+labels[key])
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
