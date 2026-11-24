package detect

import (
	"fmt"
	"regexp"
	"strings"
)

// headingKeywords qualify a README section as a candidate demo source. Matching
// is case-insensitive and by substring, so "## Quick start — Client side",
// "## Getting Started" and "## 快速开始" all qualify.
var headingKeywords = []string{
	"quick start",
	"quickstart",
	"getting started",
	"setup",
	"running",
	"how to run",
	"install",
	"demo",
	"usage",
	"快速开始",
	"快速上手",
	"安装",
	"使用",
}

// shellFenceLanguages are the fence languages that carry runnable commands.
// Anything else (xml, python, json, ...) is either a snippet or library usage.
var shellFenceLanguages = map[string]bool{
	"bash":    true,
	"sh":      true,
	"shell":   true,
	"console": true,
	"zsh":     true,
}

var (
	placeholderAngle = regexp.MustCompile(`<[^<>\s][^<>]{0,60}>`)
	placeholderYours = regexp.MustCompile(`YOUR_[A-Z0-9_]+`)
	placeholderBrace = regexp.MustCompile(`\$\{[^}\n]+\}`)

	// GPU toolchains and machine-level prerequisites mean the "demo" cannot run
	// in a throwaway container, so the block is not a runnable demo.
	gpuMarkers     = []string{"cuda", "nvcc", "cudnn", "nvidia", "tensorrt", "rocm"}
	prereqPrefixes = []string{
		"sudo ", "sudo\t", "apt-get install", "apt install", "yum install",
		"dnf install", "brew install", "systemctl", "pacman -s",
	}

	execToken = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)

	// A leading VAR=value token is an environment assignment, not a program.
	envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

	// Tokens at line start that are not evidence a demo program runs: shell
	// builtins plus output/await-only commands such as echo, printf and sleep.
	nonBinaryTokens = map[string]bool{
		"export": true, "set": true, "source": true, "cd": true,
		"unset": true, "alias": true, "echo": true, "if": true,
		"for": true, "while": true, "then": true, "fi": true,
		"printf": true, "sleep": true,
	}
)

type markdownHeading struct {
	text string
	line int
}

type markdownFence struct {
	lang string
	line int
	body []string
}

type markdownSection struct {
	heading markdownHeading
	depth   int
	fences  []markdownFence
}

// scanMarkdownSections splits a markdown document into headings and the fenced
// blocks inside each heading's section. Markdown nesting applies: a fence
// belongs to its innermost heading and to every enclosing ancestor heading, so a
// "### Step 1" block still counts for its "## Quick Start" parent. Only a
// heading of the same or shallower depth closes a section.
func scanMarkdownSections(md string) []markdownSection {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var sections []markdownSection
	var open []int
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if text, depth, ok := parseHeading(trimmed); ok {
			for len(open) > 0 && sections[open[len(open)-1]].depth >= depth {
				open = open[:len(open)-1]
			}
			sections = append(sections, markdownSection{
				heading: markdownHeading{text: text, line: i + 1},
				depth:   depth,
			})
			open = append(open, len(sections)-1)
			continue
		}
		if len(open) == 0 {
			continue
		}
		if delim, info, ok := parseFenceOpen(trimmed); ok {
			f := markdownFence{lang: fenceLanguage(info), line: i + 2}
			j := i + 1
			for ; j < len(lines); j++ {
				if isFenceClose(strings.TrimSpace(lines[j]), delim) {
					break
				}
				f.body = append(f.body, lines[j])
			}
			if len(f.body) == 0 {
				f.line = i + 1
			}
			for _, idx := range open {
				sections[idx].fences = append(sections[idx].fences, f)
			}
			i = j
		}
	}
	return sections
}

func parseHeading(trimmed string) (text string, depth int, ok bool) {
	if trimmed == "" {
		return "", 0, false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n == len(trimmed) {
		return "", 0, false
	}
	if trimmed[n] != ' ' && trimmed[n] != '\t' {
		return "", 0, false
	}
	text = strings.TrimSpace(trimmed[n:])
	text = strings.TrimRight(text, "# \t")
	if text == "" {
		return "", 0, false
	}
	return text, n, true
}

func parseFenceOpen(trimmed string) (delim, info string, ok bool) {
	for _, ch := range []byte{'`', '~'} {
		if len(trimmed) < 3 || trimmed[0] != ch {
			continue
		}
		n := 0
		for n < len(trimmed) && trimmed[n] == ch {
			n++
		}
		if n < 3 {
			return "", "", false
		}
		return strings.Repeat(string(ch), n), strings.TrimSpace(trimmed[n:]), true
	}
	return "", "", false
}

func isFenceClose(trimmed, delim string) bool {
	if len(trimmed) < len(delim) || !strings.HasPrefix(trimmed, delim) {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != delim[0] {
			return false
		}
	}
	return true
}

func fenceLanguage(info string) string {
	info = strings.TrimSpace(info)
	info = strings.Trim(info, "{}")
	if info == "" {
		return ""
	}
	if fields := strings.Fields(info); len(fields) > 0 {
		return strings.ToLower(strings.Trim(fields[0], "{}."))
	}
	return ""
}

func (f markdownFence) displayLang() string {
	if f.lang == "" {
		return "plain"
	}
	return f.lang
}

// accept applies the positive-evidence rule to one fenced block. It returns the
// runnable lines and the install lines on success, or a human reason on refusal.
func (f markdownFence) accept() (runLines, installLines []string, reason string) {
	if !shellFenceLanguages[f.lang] {
		return nil, nil, fmt.Sprintf("fence language %q is not a shell fence", f.displayLang())
	}

	body := strings.Join(f.body, "\n")
	if m := firstMatch(body); m != "" {
		return nil, nil, fmt.Sprintf("block contains placeholder %q", m)
	}
	lowered := strings.ToLower(body)
	for _, marker := range gpuMarkers {
		if strings.Contains(lowered, marker) {
			return nil, nil, fmt.Sprintf("block needs a GPU toolchain (%s)", marker)
		}
	}
	for _, line := range f.body {
		trimmed := strings.ToLower(strings.TrimSpace(line))
		for _, prefix := range prereqPrefixes {
			if strings.HasPrefix(trimmed, prefix) {
				return nil, nil, fmt.Sprintf("block needs a machine-level prerequisite (%s)", strings.TrimSpace(prefix))
			}
		}
	}

	commands := normalizedCommands(f.body)
	if len(commands) == 0 {
		return nil, nil, "block contains no commands"
	}
	// Dependency work and environment setup are removed before runnability is
	// judged, so a block that only prepares a checkout cannot become a demo.
	for _, line := range commands {
		var kept []commandSegment
		for _, seg := range splitShellSegments(line) {
			switch segmentKind(seg.text) {
			case kindInstall:
				installLines = append(installLines, seg.text)
			case kindSetup:
				// Setup is neither demo evidence nor an image install.
			default:
				kept = append(kept, seg)
			}
		}
		if len(kept) > 0 {
			runLines = append(runLines, joinShellSegments(kept))
		}
	}
	if len(runLines) == 0 {
		return nil, nil, "block only installs dependencies or sets up the environment"
	}
	if !invokesBinary(runLines) {
		return nil, nil, "block has no binary invoked at line start"
	}
	return runLines, installLines, ""
}

// invokesBinary reports whether any run segment starts by invoking a real
// program, so an install-then-echo block is not mistaken for a demo.
func invokesBinary(runLines []string) bool {
	for _, line := range runLines {
		for _, seg := range splitShellSegments(line) {
			token := invocationToken(seg.text)
			if token != "" && execToken.MatchString(token) && !nonBinaryTokens[token] {
				return true
			}
		}
	}
	return false
}

// invocationToken returns the program a command line invokes at line start,
// skipping leading VAR=value assignments and any leading path, so "./run.sh",
// "/usr/bin/env python" and "FOO=1 python" all name a program.
func invocationToken(line string) string {
	fields := strings.Fields(line)
	i := 0
	for i < len(fields) && envAssignment.MatchString(fields[i]) {
		i++
	}
	if i == len(fields) {
		return ""
	}
	program := fields[i]
	return program[strings.LastIndex(program, "/")+1:]
}

func firstMatch(body string) string {
	for _, re := range []*regexp.Regexp{placeholderAngle, placeholderYours, placeholderBrace} {
		if m := re.FindString(body); m != "" {
			return m
		}
	}
	return ""
}

// normalizedCommands strips prompts and comments, leaving executable lines.
func normalizedCommands(body []string) []string {
	var out []string
	for _, raw := range body {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "$ ")
		line = strings.TrimPrefix(line, "> ")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// lineKind classifies one shell segment by what it contributes to a container
// run. Only kindRun can be demo evidence; install and setup work belongs to the
// image build, so neither may stand in for a demo.
type lineKind int

const (
	kindRun lineKind = iota
	kindInstall
	kindSetup
)

// installFamilies maps each dependency-management program to the subcommands
// that only fetch, install, lock or build dependencies. Keeping the knowledge in
// one table keeps the rule structural: a line is dependency work when its
// program is a family and its first argument is a non-demo subcommand.
var installFamilies = map[string]map[string]bool{
	"npm":      wordSet("ci", "i", "install", "add", "update", "dedupe", "prune", "audit", "fund", "rebuild"),
	"pnpm":     wordSet("i", "install", "add", "update", "fetch", "import", "prune"),
	"yarn":     wordSet("install", "add", "upgrade", "fetch", "init"),
	"bun":      wordSet("i", "install", "add", "update"),
	"composer": wordSet("install", "require", "update", "dump-autoload"),
	"bundle":   wordSet("install", "update", "add"),
	"gem":      wordSet("install", "update"),
	"conda":    wordSet("install", "create", "update", "env"),
	"mamba":    wordSet("install", "create", "update", "env"),
	"uv":       wordSet("sync", "add", "pip", "lock", "init", "venv"),
	"pip":      wordSet("install", "download", "wheel", "uninstall"),
	"pip3":     wordSet("install", "download", "wheel", "uninstall"),
	"pipx":     wordSet("install", "inject", "upgrade", "ensurepath"),
	"poetry":   wordSet("install", "add", "update", "lock", "shell", "env", "config"),
	"go":       wordSet("mod", "get"),
	"cargo":    wordSet("fetch", "install", "add", "update", "generate-lockfile"),
	"apt":      wordSet("install", "update", "upgrade"),
	"apt-get":  wordSet("install", "update", "upgrade"),
	"brew":     wordSet("install", "tap", "link"),
	"make":     wordSet("install"),
	"mvn":      wordSet("install", "package", "compile", "clean", "test", "verify", "dependency", "wrapper"),
	"gradle":   wordSet("wrapper", "build", "assemble", "install", "dependencies"),
}

// bareInstallFamilies are package managers whose argument-less form installs.
var bareInstallFamilies = map[string]bool{"yarn": true}

// setupTokens activate or configure an environment instead of running a program.
var setupTokens = map[string]bool{
	"source": true, ".": true, "export": true, "virtualenv": true,
}

func wordSet(words ...string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return set
}

// commandSegment is one command inside a shell line, with the operator that
// introduced it ("&&", "||", ";", "|" or "" at line start).
type commandSegment struct {
	text string
	sep  string
}

// splitShellSegments splits a line on shell control operators, leaving quoted
// text and redirections intact.
func splitShellSegments(line string) []commandSegment {
	var (
		out   []commandSegment
		buf   strings.Builder
		sep   string
		quote byte
	)
	flush := func(next string) {
		text := strings.TrimSpace(buf.String())
		buf.Reset()
		if text != "" {
			out = append(out, commandSegment{text: text, sep: sep})
		}
		sep = next
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			buf.WriteByte(c)
			switch {
			case c == quote:
				quote = 0
			case c == '\\' && quote == '"' && i+1 < len(line):
				i++
				buf.WriteByte(line[i])
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			buf.WriteByte(c)
		case '\\':
			buf.WriteByte(c)
			if i+1 < len(line) {
				i++
				buf.WriteByte(line[i])
			}
		case '&':
			if i+1 < len(line) && line[i+1] == '&' {
				flush("&&")
				i++
			} else {
				buf.WriteByte(c)
			}
		case '|':
			if i+1 < len(line) && line[i+1] == '|' {
				flush("||")
				i++
			} else {
				flush("|")
			}
		case ';':
			flush(";")
		default:
			buf.WriteByte(c)
		}
	}
	flush("")
	return out
}

// joinShellSegments rebuilds the runnable remainder, keeping original operators
// between segments that survived classification.
func joinShellSegments(segs []commandSegment) string {
	var b strings.Builder
	for i, seg := range segs {
		if i > 0 {
			b.WriteString(" ")
			if seg.sep != "" {
				b.WriteString(seg.sep)
				b.WriteString(" ")
			}
		}
		b.WriteString(seg.text)
	}
	return b.String()
}

// segmentKind classifies one command segment as a demo run, dependency work or
// environment setup.
func segmentKind(text string) lineKind {
	fields := strings.Fields(text)
	i := 0
	for i < len(fields) && envAssignment.MatchString(fields[i]) {
		i++
	}
	if i == len(fields) {
		return kindSetup
	}
	program := fields[i]
	program = program[strings.LastIndex(program, "/")+1:]
	args := fields[i+1:]

	if setupTokens[program] {
		return kindSetup
	}
	if program == "python" || program == "python3" || program == "py" {
		module, rest, ok := pythonModule(args)
		if !ok {
			return kindRun
		}
		switch module {
		case "venv", "virtualenv":
			return kindSetup
		case "pip", "pip3":
			if installFamilies["pip"][firstNonFlagArg(rest)] {
				return kindInstall
			}
		}
		return kindRun
	}
	family, ok := installFamilies[program]
	if !ok {
		return kindRun
	}
	sub := firstNonFlagArg(args)
	if sub == "" {
		if bareInstallFamilies[program] {
			return kindInstall
		}
		return kindRun
	}
	if family[sub] {
		return kindInstall
	}
	return kindRun
}

// pythonModule finds `-m <module>` in a python argument list.
func pythonModule(args []string) (module string, rest []string, ok bool) {
	for i := 0; i < len(args); i++ {
		if args[i] != "-m" || i+1 >= len(args) {
			continue
		}
		return args[i+1], args[i+2:], true
	}
	return "", nil, false
}

func firstNonFlagArg(args []string) string {
	for _, arg := range args {
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

func headingQualifies(text string) bool {
	lowered := strings.ToLower(text)
	for _, kw := range headingKeywords {
		if strings.Contains(lowered, kw) {
			return true
		}
	}
	return false
}

// demoFromReadme implements the README fallback: only a fence inside a
// quickstart-style heading is considered, and only with positive runnability
// signals. When nothing qualifies it returns a reason, never an error.
func demoFromReadme(md string) (demo, install []string, evidence, reason string) {
	var firstReason string
	for _, section := range scanMarkdownSections(md) {
		if !headingQualifies(section.heading.text) {
			continue
		}
		if len(section.fences) == 0 {
			if firstReason == "" {
				firstReason = fmt.Sprintf("README %q has no fenced code block", section.heading.text)
			}
			continue
		}
		for i, fence := range section.fences {
			runLines, installLines, why := fence.accept()
			if why == "" {
				evidence = fmt.Sprintf("README:%s > %s fence @L%d", section.heading.text, fence.displayLang(), fence.line)
				return []string{"sh", "-lc", strings.Join(runLines, "\n")}, installLines, evidence, ""
			}
			if firstReason == "" {
				firstReason = fmt.Sprintf("README %q block %d: %s", section.heading.text, i+1, why)
			}
		}
	}
	if firstReason == "" {
		firstReason = "README has no quickstart-style heading"
	}
	return nil, nil, "", firstReason
}
