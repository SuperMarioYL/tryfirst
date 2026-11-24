// Package detect derives a runnable plan from what a repository declares:
// package.json, pyproject.toml, requirements.txt, a README quickstart section,
// or a SKILL.md that documents a prompt-only skill.
package detect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/SuperMarioYL/tryfirst/internal/plan"
)

// Base images used for each detected runtime.
const (
	pythonBaseImage = "python:3.12-slim"
	nodeBaseImage   = "node:22-slim"
	shellBaseImage  = "debian:bookworm-slim"

	pythonRuntime = "python-3.12"
	nodeRuntime   = "node-22"
	shellRuntime  = "shell"
)

// nodeScriptPreference lists package.json scripts in the order a documented
// demo usually hides in.
var nodeScriptPreference = []string{"demo", "start", "dev", "serve", "example", "run"}

// Detect inspects the checkout at dir and returns a run plan. A repository
// without an executable demo is not an error: NoDemoReason says why.
func Detect(dir string) (plan.RunPlan, error) {
	var p runPlan
	if err := p.manifest(dir); err != nil {
		return plan.RunPlan{}, err
	}
	if p.demo == nil {
		md, err := readFirst(dir, "README.md", "README.MD", "readme.md", "README.markdown", "README")
		if err != nil {
			return plan.RunPlan{}, err
		}
		if md != "" {
			demo, install, evidence, reason := demoFromReadme(md)
			if demo != nil {
				p.demo = demo
				p.evidence = evidence
				p.install = mergeCommands(p.install, install)
				if p.runtime == "" {
					p.runtime, p.baseImage = runtimeForCommands(demo)
				}
			} else {
				p.reason = reason
			}
		}
	}

	if p.demo == nil {
		if p.reason == "" {
			p.reason = "no package.json, pyproject.toml, requirements.txt or README quickstart found"
		}
		if note := skillNote(dir); note != "" {
			p.reason = p.reason + "; " + note
		}
	}

	out := plan.RunPlan{
		Runtime:      p.runtime,
		BaseImage:    p.baseImage,
		Install:      p.install,
		DemoCommand:  p.demo,
		Evidence:     p.evidence,
		NoDemoReason: p.reason,
	}
	if out.Runnable() {
		out.NoDemoReason = ""
	}
	return out, nil
}

// runPlan accumulates detection state before it becomes a plan.RunPlan.
type runPlan struct {
	runtime   string
	baseImage string
	install   []string
	demo      []string
	evidence  string
	reason    string
}

// manifest fills the plan from package.json, pyproject.toml or
// requirements.txt, in that order of preference. A manifest without an entry
// point still contributes runtime and install commands; the README fallback may
// supply the demo.
func (p *runPlan) manifest(dir string) error {
	if data, ok, err := readOptional(filepath.Join(dir, "package.json")); err != nil {
		return err
	} else if ok {
		p.node(dir, data)
		return nil
	}
	if data, ok, err := readOptional(filepath.Join(dir, "pyproject.toml")); err != nil {
		return err
	} else if ok {
		p.pythonManifest(dir, data)
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
		p.runtime, p.baseImage = pythonRuntime, pythonBaseImage
		p.install = []string{"pip install -r requirements.txt"}
		p.reason = "requirements.txt declares no entry point"
	}
	return nil
}

func (p *runPlan) node(dir string, data []byte) {
	p.runtime, p.baseImage = nodeRuntime, nodeBaseImage
	if _, err := os.Stat(filepath.Join(dir, "package-lock.json")); err == nil {
		p.install = []string{"npm ci"}
	} else {
		p.install = []string{"npm install"}
	}

	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		p.reason = "package.json is not valid JSON"
		return
	}
	for _, name := range nodeScriptPreference {
		if _, ok := pkg.Scripts[name]; ok {
			p.demo = []string{"npm", "run", name}
			p.evidence = "package.json:scripts." + name
			return
		}
	}
	p.reason = "package.json declares no demo or start script"
}

func (p *runPlan) pythonManifest(dir string, data []byte) {
	p.runtime, p.baseImage = pythonRuntime, pythonBaseImage

	hasRequirements := false
	if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
		hasRequirements = true
		p.install = append(p.install, "pip install -r requirements.txt")
	}
	p.install = append(p.install, "pip install .")

	scripts, section := pyprojectScripts(string(data))
	if len(scripts) > 0 {
		sort.Strings(scripts)
		name := scripts[0]
		p.demo = []string{name}
		p.evidence = fmt.Sprintf("pyproject.toml:%s.%s", section, name)
		return
	}
	switch {
	case hasRequirements:
		p.reason = "requirements.txt declares no entry point"
	default:
		p.reason = "pyproject.toml declares no entry point"
	}
}

// pyprojectScripts extracts console script names from [project.scripts] or
// [tool.poetry.scripts]. TOML is read by hand because the plan allows only one
// dependency, and these sections are simple key/value tables.
func pyprojectScripts(toml string) (names []string, section string) {
	current := ""
	for _, line := range strings.Split(strings.ReplaceAll(toml, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			current = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			continue
		}
		if current != "project.scripts" && current != "tool.poetry.scripts" {
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if !ok || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key = strings.Trim(strings.TrimSpace(key), `"'`)
		if key == "" {
			continue
		}
		names = append(names, key)
		section = "[" + current + "]"
	}
	return names, section
}

// skillNote describes a SKILL.md when it is the only thing explaining the repo.
func skillNote(dir string) string {
	data, ok, err := readOptional(filepath.Join(dir, "SKILL.md"))
	if err != nil || !ok {
		return ""
	}
	frontmatter, found := ParseSkillFrontmatter(data)
	if !found {
		return "SKILL.md declares no executable demo"
	}
	if name, ok := frontmatter["name"].(string); ok && name != "" {
		return fmt.Sprintf("SKILL.md (%q) declares no executable demo", name)
	}
	return "SKILL.md declares no executable demo"
}

// ParseSkillFrontmatter reads YAML frontmatter from a SKILL.md body. Parsing is
// tolerant: missing frontmatter is fine (found=false), unknown keys are ignored
// because they land in a plain map, and malformed YAML is treated as absent
// frontmatter rather than a parse failure.
func ParseSkillFrontmatter(data []byte) (map[string]any, bool) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.TrimSpace(line) != "---" {
			return nil, false
		}
		start = i
		break
	}
	if start < 0 {
		return nil, false
	}
	end := -1
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, false
	}
	frontmatter := map[string]any{}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[start+1:end], "\n")), &frontmatter); err != nil {
		return nil, false
	}
	return frontmatter, true
}

// runtimeForCommands infers the base image from the commands a README block
// runs, so a README-only repository still gets a sensible sandbox.
func runtimeForCommands(demo []string) (runtime, baseImage string) {
	script := strings.Join(demo, " ")
	fields := strings.Fields(script)
	for _, field := range fields {
		switch {
		case field == "python" || field == "python3" || field == "pip" || field == "pip3" || field == "uv":
			return pythonRuntime, pythonBaseImage
		case field == "node" || field == "npm" || field == "npx" || field == "yarn" || field == "pnpm" || field == "bun":
			return nodeRuntime, nodeBaseImage
		}
	}
	return shellRuntime, shellBaseImage
}

func mergeCommands(base, extra []string) []string {
	seen := make(map[string]bool, len(base))
	out := make([]string, 0, len(base)+len(extra))
	for _, cmd := range append(append([]string{}, base...), extra...) {
		if cmd == "" || seen[cmd] {
			continue
		}
		seen[cmd] = true
		out = append(out, cmd)
	}
	return out
}

func readOptional(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

func readFirst(dir string, names ...string) (string, error) {
	for _, name := range names {
		data, ok, err := readOptional(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		if ok {
			return string(data), nil
		}
	}
	return "", nil
}
