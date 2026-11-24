// Package cli wires the commands: plan, run, gc, version, and the hidden
// __stub server that runs inside the sandbox.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/SuperMarioYL/tryfirst/internal/detect"
	"github.com/SuperMarioYL/tryfirst/internal/gate"
	"github.com/SuperMarioYL/tryfirst/internal/plan"
	"github.com/SuperMarioYL/tryfirst/internal/receipt"
	"github.com/SuperMarioYL/tryfirst/internal/sandbox"
)

const usageText = `tryfirst — try a repository's own demo in a throwaway sandbox.

usage:
  tryfirst plan <repo> [--json] [--out plan.json]
  tryfirst run  <repo> [--model stub|live] [--timeout 180s] [--report-dir tryfirst-report] [--keep]
  tryfirst gc
  tryfirst version

<repo> is a git URL or a local directory. Docker must be running for "run".
Receipts always land in --report-dir; --keep also leaves the per-run clone in
place for inspection.
Live model mode reads TRYFIRST_UPSTREAM_BASE_URL and TRYFIRST_UPSTREAM_API_KEY
from the operator environment; with either missing it stops before a container
starts. Stub mode needs no credential.
`

// Main dispatches one CLI invocation and returns the process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return 2
	}
	switch args[0] {
	case "plan":
		return cmdPlan(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "gc":
		return cmdGC(args[1:])
	case "version", "--version", "-v":
		fmt.Println(Version())
		return 0
	case "__stub":
		return cmdStub(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return 0
	}
	fmt.Fprintf(os.Stderr, "tryfirst: unknown command %q\n\n%s", args[0], usageText)
	return 2
}

// Version reports the module version recorded by the Go toolchain.
func Version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return "0.1.0"
}

func cmdPlan(args []string) int {
	var repoArg, outPath string
	outPath = "plan.json"
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--json":
			asJSON = true
		case arg == "--out":
			if i+1 >= len(args) {
				return flagError("--out needs a path")
			}
			i++
			outPath = args[i]
		case arg == "-h" || arg == "--help":
			fmt.Print(usageText)
			return 0
		case strings.HasPrefix(arg, "-"):
			return flagError("unknown flag " + arg)
		default:
			if repoArg != "" {
				return flagError("plan takes one repository")
			}
			repoArg = arg
		}
	}
	if repoArg == "" {
		return flagError("plan needs a repository")
	}

	ws, err := prepare(repoArg)
	if err != nil {
		return fail(err)
	}
	defer ws.cleanup()

	detected, err := detect.Detect(ws.dir)
	if err != nil {
		return fail(err)
	}
	detected.Repo = ws.display
	detected.Commit = ws.commit

	if asJSON {
		encoded, err := json.MarshalIndent(detected, "", "  ")
		if err != nil {
			return fail(err)
		}
		if err := os.WriteFile(outPath, append(encoded, '\n'), 0o644); err != nil {
			return fail(err)
		}
		fmt.Println(string(encoded))
		return 0
	}

	fmt.Printf("repo:     %s\n", detected.Repo)
	fmt.Printf("commit:   %s\n", detected.Commit)
	fmt.Printf("runtime:  %s\n", detected.Runtime)
	fmt.Printf("image:    %s\n", detected.BaseImage)
	for _, cmd := range detected.Install {
		fmt.Printf("install:  %s\n", cmd)
	}
	if detected.Runnable() {
		fmt.Printf("demo:     %s\n", detected.DemoScript())
		fmt.Printf("evidence: %s\n", detected.Evidence)
		fmt.Printf("verdict:  %s\n", plan.VerdictPlanOnly)
		return 0
	}
	fmt.Printf("verdict:  %s\n", plan.VerdictNoRunnableDemo)
	fmt.Printf("reason:   %s\n", detected.NoDemoReason)
	return 0
}

type runOptions struct {
	model     string
	timeout   time.Duration
	reportDir string
	keep      bool
}

func cmdRun(args []string) int {
	opts := runOptions{model: plan.ModelStub, timeout: 180 * time.Second, reportDir: "tryfirst-report"}
	var repoArg string
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--model":
			if i+1 >= len(args) {
				return flagError("--model needs stub or live")
			}
			i++
			opts.model = args[i]
		case arg == "--timeout":
			if i+1 >= len(args) {
				return flagError("--timeout needs a duration")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d <= 0 {
				return flagError("invalid --timeout " + args[i])
			}
			opts.timeout = d
		case arg == "--report-dir":
			if i+1 >= len(args) {
				return flagError("--report-dir needs a path")
			}
			i++
			opts.reportDir = args[i]
		case arg == "--keep":
			opts.keep = true
		case arg == "-h" || arg == "--help":
			fmt.Print(usageText)
			return 0
		case strings.HasPrefix(arg, "-"):
			return flagError("unknown flag " + arg)
		default:
			if repoArg != "" {
				return flagError("run takes one repository")
			}
			repoArg = arg
		}
	}
	if repoArg == "" {
		return flagError("run needs a repository")
	}
	if opts.model != plan.ModelStub && opts.model != plan.ModelLive {
		return flagError("--model must be stub or live")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Live mode fails closed before the repository is touched, let alone a
	// container started.
	var live *gate.Gate
	if opts.model == plan.ModelLive {
		var err error
		live, err = gate.FromEnvironment()
		if err != nil {
			return fail(err)
		}
	}

	ws, err := prepare(repoArg)
	if err != nil {
		return fail(err)
	}
	if opts.keep {
		fmt.Printf("work dir kept: %s\n", ws.dir)
	} else {
		defer ws.cleanup()
	}

	started := time.Now().UTC()
	detected, err := detect.Detect(ws.dir)
	if err != nil {
		return fail(err)
	}
	detected.Repo = ws.display
	detected.Commit = ws.commit

	rep := receipt.Receipt{
		Schema:    receipt.Schema,
		RunID:     newRunID(),
		StartedAt: started,
		Plan:      detected,
		ModelMode: opts.model,
		Network:   "none",
	}

	if !detected.Runnable() {
		rep.Verdict = plan.VerdictNoRunnableDemo
		rep.ExitCode = -1
		rep.DurationMS = time.Since(started).Milliseconds()
		return persist(&rep, opts, ws)
	}

	runner := sandbox.NewDocker()
	if err := runner.Ping(ctx); err != nil {
		return fail(err)
	}

	stubDir, err := os.MkdirTemp("", "tryfirst-stub-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(stubDir)
	arch, err := runner.ServerArch(ctx)
	if err != nil {
		return fail(err)
	}
	stubPath, err := sandbox.BuildLinuxStub(stubDir, arch)
	if err != nil {
		return fail(err)
	}

	// The receipt records "gate" for live runs; docker needs "bridge".
	dockerNetwork := "none"
	var modelEnv map[string]string
	if live != nil {
		if err := live.Start(); err != nil {
			return fail(err)
		}
		defer live.Close()
		modelEnv = sandbox.LiveModelEnv(gate.HostGatewayBase(live.Port()), live.Token())
		rep.Network = "gate"
		dockerNetwork = "bridge"
	} else {
		modelEnv = sandbox.StubModelEnv()
	}
	rep.Plan.ModelEnv = modelEnv

	session := sandbox.Session{
		Runner:   runner,
		Plan:     detected,
		WorkDir:  ws.dir,
		ImageRef: sandbox.ImageRef(ws.commit, rep.RunID),
		ModelEnv: modelEnv,
		Network:  dockerNetwork,
		Timeout:  opts.timeout,
		RunID:    rep.RunID,
		StubPath: stubPath,
	}
	result, err := session.Execute(ctx)
	if err != nil {
		return fail(err)
	}

	rep.Stdout = result.Stdout
	rep.ExitCode = result.ExitCode
	rep.DurationMS = result.DurationMS
	if live != nil {
		rep.ModelCalls = live.ModelCalls()
		rep.CapHit = live.CapHit()
		rep.Stderr = result.Stderr
	} else {
		rep.ModelCalls, rep.Stderr = sandbox.ParseStubStats(result.Stderr)
	}
	if result.TimedOut {
		rep.Stderr = strings.TrimRight(rep.Stderr, "\n") + fmt.Sprintf("\n[tryfirst] demo exceeded %s and was killed\n", opts.timeout)
	}
	if result.ExitCode == 0 {
		rep.Verdict = plan.VerdictRuns
	} else {
		rep.Verdict = plan.VerdictFails
	}
	return persist(&rep, opts, ws)
}

// persist writes the receipt and prints the human summary.
func persist(rep *receipt.Receipt, opts runOptions, ws *workspace) int {
	base := receipt.BaseName(ws.owner, ws.name, ws.commit)
	paths, err := receipt.Write(opts.reportDir, base, rep)
	if err != nil {
		return fail(err)
	}

	if rep.Verdict == plan.VerdictNoRunnableDemo {
		fmt.Printf("verdict: %s\n", rep.Verdict)
		fmt.Printf("reason:  %s\n", rep.Plan.NoDemoReason)
		fmt.Printf("receipt: %s\n", paths.JSON)
		return 0
	}

	fmt.Printf("detected: %s · demo source: %s\n", rep.Plan.Runtime, describeSource(rep.Plan))
	fmt.Printf("sandbox: %s · network: %s · model: %s\n", rep.Plan.BaseImage, rep.Network, rep.ModelMode)
	fmt.Println(separator("demo output"))
	fmt.Print(rep.Stdout)
	if rep.Stdout != "" && !strings.HasSuffix(rep.Stdout, "\n") {
		fmt.Println()
	}
	if strings.TrimSpace(rep.Stderr) != "" {
		fmt.Print(rep.Stderr)
		if !strings.HasSuffix(rep.Stderr, "\n") {
			fmt.Println()
		}
	}
	fmt.Println(separator("verdict"))
	if rep.CapHit {
		fmt.Printf("%s · exit %d · %.1fs · model cap hit\n", rep.Verdict, rep.ExitCode, float64(rep.DurationMS)/1000)
	} else {
		fmt.Printf("%s · exit %d · %.1fs\n", rep.Verdict, rep.ExitCode, float64(rep.DurationMS)/1000)
	}
	fmt.Printf("receipt: %s\n", paths.JSON)
	fmt.Printf("report:  %s\n", paths.HTML)
	return 0
}

func describeSource(p plan.RunPlan) string {
	if p.NoDemoReason != "" {
		return "none"
	}
	if p.Evidence == "" {
		return "declared"
	}
	return p.Evidence
}

func separator(title string) string {
	const width = 56
	line := "── " + title + " "
	if pad := width - len([]rune(line)); pad > 0 {
		line += strings.Repeat("─", pad)
	}
	return line
}

func cmdGC(args []string) int {
	for _, arg := range args {
		switch arg {
		case "-h", "--help":
			fmt.Print(usageText)
			return 0
		default:
			return flagError("unknown flag " + arg)
		}
	}
	const maxAge = 7 * 24 * time.Hour
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	out, err := sandbox.NewDocker().PruneImages(ctx, sandbox.ManagedLabel, maxAge)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("pruned tryfirst images older than %s\n", maxAge)
	if trimmed := strings.TrimSpace(out); trimmed != "" {
		fmt.Println(trimmed)
	}
	return 0
}

func cmdStub(args []string) int {
	addr, ready, stats := plan.StubAddr, plan.StubReadyFile, plan.StubStatsFile
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--addr", "--ready", "--stats":
			if i+1 >= len(args) {
				return flagError(arg + " needs a value")
			}
			i++
			switch arg {
			case "--addr":
				addr = args[i]
			case "--ready":
				ready = args[i]
			case "--stats":
				stats = args[i]
			}
		default:
			return flagError("unknown flag " + arg)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := sandbox.ServeStub(ctx, addr, ready, stats); err != nil {
		fmt.Fprintln(os.Stderr, "tryfirst stub:", err)
		return 1
	}
	return 0
}

type workspace struct {
	dir     string
	commit  string
	display string
	owner   string
	name    string
	cleanup func()
}

// prepare materialises the repository in a temp dir. A local directory is
// copied, so helper files never touch the user's tree; a URL is cloned.
func prepare(repoArg string) (*workspace, error) {
	tmp, err := os.MkdirTemp("", "tryfirst-work-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	dest := filepath.Join(tmp, "repo")

	if info, statErr := os.Stat(repoArg); statErr == nil && info.IsDir() {
		if err := copyTree(repoArg, dest); err != nil {
			cleanup()
			return nil, err
		}
		return &workspace{
			dir:     dest,
			commit:  shortCommit(repoArg),
			display: repoArg,
			owner:   "local",
			name:    filepath.Base(filepath.Clean(repoArg)),
			cleanup: cleanup,
		}, nil
	}

	owner, name, err := splitRepo(repoArg)
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := gitClone(repoArg, dest); err != nil {
		cleanup()
		return nil, err
	}
	return &workspace{
		dir:     dest,
		commit:  shortCommit(dest),
		display: repoArg,
		owner:   owner,
		name:    name,
		cleanup: cleanup,
	}, nil
}

func splitRepo(repoArg string) (owner, name string, err error) {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(repoArg, "/"), ".git")
	var path string
	switch {
	case strings.Contains(trimmed, "://"):
		rest := trimmed[strings.Index(trimmed, "://")+3:]
		if idx := strings.Index(rest, "/"); idx >= 0 {
			path = rest[idx+1:]
		}
	case strings.HasPrefix(trimmed, "git@"):
		if idx := strings.Index(trimmed, ":"); idx >= 0 {
			path = trimmed[idx+1:]
		}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("cannot derive owner/repo from %q", repoArg)
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}

func gitClone(repoArg, dest string) error {
	cmd := exec.Command("git", "clone", "--depth", "1", "--quiet", repoArg, dest)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s: %v: %s", repoArg, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func shortCommit(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--short=8", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "nogit"
	}
	return strings.TrimSpace(string(out))
}

func copyTree(src, dest string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dest, 0o755)
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func newRunID() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(buf)
}

func flagError(message string) int {
	fmt.Fprintf(os.Stderr, "tryfirst: %s\n", message)
	return 2
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "tryfirst: %v\n", err)
	return 1
}
