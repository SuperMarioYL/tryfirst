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

	// Shell keywords are not evidence that a binary is invoked.
	nonBinaryTokens = map[string]bool{
		"export": true, "set": true, "source": true, "cd": true,
		"unset": true, "alias": true, "echo": true, "if": true,
		"for": true, "while": true, "then": true, "fi": true,
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
	fences  []markdownFence
}

// scanMarkdownSections splits a markdown document into headings and the fenced
// blocks inside each heading's section.
func scanMarkdownSections(md string) []markdownSection {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var sections []markdownSection
	current := -1
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if text, ok := parseHeading(trimmed); ok {
			sections = append(sections, markdownSection{heading: markdownHeading{text: text, line: i + 1}})
			current = len(sections) - 1
			continue
		}
		if current < 0 {
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
			sections[current].fences = append(sections[current].fences, f)
			i = j
		}
	}
	return sections
}

func parseHeading(trimmed string) (string, bool) {
	if trimmed == "" {
		return "", false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n == len(trimmed) {
		return "", false
	}
	if trimmed[n] != ' ' && trimmed[n] != '\t' {
		return "", false
	}
	text := strings.TrimSpace(trimmed[n:])
	text = strings.TrimRight(text, "# \t")
	if text == "" {
		return "", false
	}
	return text, true
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
	hasBinary := false
	for _, line := range commands {
		token := strings.Fields(line)[0]
		if execToken.MatchString(token) && !nonBinaryTokens[token] {
			hasBinary = true
		}
		if isInstallLine(line) {
			installLines = append(installLines, line)
			continue
		}
		runLines = append(runLines, line)
	}
	if !hasBinary {
		return nil, nil, "block has no binary invoked at line start"
	}
	if len(runLines) == 0 {
		return nil, nil, "block only installs dependencies"
	}
	return runLines, installLines, ""
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

func isInstallLine(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "pip", "pip3", "npm", "yarn", "pnpm", "bun", "cargo", "apt-get", "apt", "poetry":
		return hasWord(fields, "install") || hasWord(fields, "add") || hasWord(fields, "sync")
	case "uv":
		return hasWord(fields, "sync") || (len(fields) > 2 && fields[1] == "pip" && fields[2] == "install")
	case "python", "python3":
		return len(fields) > 3 && fields[1] == "-m" && fields[2] == "pip" && fields[3] == "install"
	}
	return false
}

func hasWord(fields []string, word string) bool {
	for _, f := range fields[1:] {
		if f == word {
			return true
		}
	}
	return false
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
