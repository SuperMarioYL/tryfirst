package detect_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SuperMarioYL/tryfirst/internal/detect"
	"github.com/SuperMarioYL/tryfirst/internal/plan"
)

func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func mustDetect(t *testing.T, files map[string]string) plan.RunPlan {
	t.Helper()
	got, err := detect.Detect(writeFixture(t, files))
	if err != nil {
		t.Fatalf("Detect returned an error, but detection problems are verdicts: %v", err)
	}
	return got
}

func equalStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %#v, want %#v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %#v, want %#v", label, got, want)
		}
	}
}

// Fixture 1: a node repo whose package.json declares a demo script.
func TestDetectNodeDemo(t *testing.T) {
	p := mustDetect(t, map[string]string{
		"package.json": `{"name":"demo","scripts":{"demo":"node index.js","start":"node server.js"}}`,
		"index.js":     "console.log('hello')\n",
	})
	if !p.Runnable() {
		t.Fatalf("expected a runnable plan, got no_demo_reason %q", p.NoDemoReason)
	}
	if p.Runtime != "node-22" || p.BaseImage != "node:22-slim" {
		t.Fatalf("runtime/base = %q/%q", p.Runtime, p.BaseImage)
	}
	equalStrings(t, "install", p.Install, []string{"npm install"})
	equalStrings(t, "demo", p.DemoCommand, []string{"npm", "run", "demo"})
	if p.Evidence != "package.json:scripts.demo" {
		t.Fatalf("evidence = %q", p.Evidence)
	}
}

func TestDetectNodeUsesLockfile(t *testing.T) {
	p := mustDetect(t, map[string]string{
		"package.json":      `{"scripts":{"start":"node server.js"}}`,
		"package-lock.json": `{}`,
	})
	equalStrings(t, "install", p.Install, []string{"npm ci"})
	equalStrings(t, "demo", p.DemoCommand, []string{"npm", "run", "start"})
}

// Fixture 2: a python repo that declares a console entry point.
func TestDetectPythonEntryPoint(t *testing.T) {
	p := mustDetect(t, map[string]string{
		"pyproject.toml": `[build-system]
requires = ["setuptools>=61"]

[project]
name = "hello"
version = "0.1.0"

[project.scripts]
hello = "hello:main"
`,
		"hello.py": "def main():\n    print('hi')\n",
	})
	if !p.Runnable() {
		t.Fatalf("expected a runnable plan, got %q", p.NoDemoReason)
	}
	if p.Runtime != "python-3.12" || p.BaseImage != "python:3.12-slim" {
		t.Fatalf("runtime/base = %q/%q", p.Runtime, p.BaseImage)
	}
	equalStrings(t, "install", p.Install, []string{"pip install ."})
	equalStrings(t, "demo", p.DemoCommand, []string{"hello"})
	if p.Evidence != "pyproject.toml:[project.scripts].hello" {
		t.Fatalf("evidence = %q", p.Evidence)
	}
}

func TestDetectPoetryScripts(t *testing.T) {
	p := mustDetect(t, map[string]string{
		"pyproject.toml": `[tool.poetry]
name = "x"

[tool.poetry.scripts]
x-demo = "x:cli"
`,
		"requirements.txt": "requests\n",
	})
	equalStrings(t, "install", p.Install, []string{
		"pip install -r requirements.txt",
		"pip install .",
	})
	equalStrings(t, "demo", p.DemoCommand, []string{"x-demo"})
	if !strings.Contains(p.Evidence, "[tool.poetry.scripts]") {
		t.Fatalf("evidence = %q", p.Evidence)
	}
}

func TestDetectRequirementsWithoutEntryPoint(t *testing.T) {
	p := mustDetect(t, map[string]string{"requirements.txt": "requests\n"})
	if p.Runnable() || !strings.Contains(p.NoDemoReason, "entry point") {
		t.Fatalf("expected no_runnable_demo with an entry-point reason, got %q", p.NoDemoReason)
	}
	if p.Runtime != "python-3.12" {
		t.Fatalf("runtime = %q", p.Runtime)
	}
}

// Fixture 3: a README-only repo with an accepted shell block.
func TestDetectReadmeShellBlock(t *testing.T) {
	p := mustDetect(t, map[string]string{
		"README.md": `# Example

A tool.

## Quick start

Install and run:

` + "```bash" + `
pip install -r requirements.txt
python demo.py --once
` + "```" + `
`,
	})
	if !p.Runnable() {
		t.Fatalf("expected a runnable plan, got %q", p.NoDemoReason)
	}
	equalStrings(t, "install", p.Install, []string{"pip install -r requirements.txt"})
	equalStrings(t, "demo", p.DemoCommand, []string{"sh", "-lc", "python demo.py --once"})
	if !strings.HasPrefix(p.Evidence, "README:Quick start > bash fence @L") {
		t.Fatalf("evidence = %q", p.Evidence)
	}
	if p.Runtime != "python-3.12" || p.BaseImage != "python:3.12-slim" {
		t.Fatalf("runtime/base inferred from commands = %q/%q", p.Runtime, p.BaseImage)
	}
}

func TestDetectReadmeConsolePrompt(t *testing.T) {
	p := mustDetect(t, map[string]string{
		"README.md": "## Usage\n\n```console\n$ node demo.js\n```\n",
	})
	equalStrings(t, "demo", p.DemoCommand, []string{"sh", "-lc", "node demo.js"})
	if p.Runtime != "node-22" {
		t.Fatalf("runtime = %q", p.Runtime)
	}
}

func TestDetectReadmeHeadingVariants(t *testing.T) {
	headings := []string{
		"## Quick Start",
		"## Quick start — Client side",
		"## Getting Started",
		"## Setup",
		"## Running",
		"## How to Run",
		"## Install (macOS)",
		"## Demo",
		"## Usage",
		"## 快速开始",
		"## 快速上手",
		"## 安装",
		"## 使用",
	}
	for _, heading := range headings {
		t.Run(heading, func(t *testing.T) {
			md := "# Repo\n\n" + heading + "\n\n```bash\npython run.py\n```\n"
			p := mustDetect(t, map[string]string{"README.md": md})
			if !p.Runnable() {
				t.Fatalf("heading %q was not accepted: %s", heading, p.NoDemoReason)
			}
			equalStrings(t, "demo", p.DemoCommand, []string{"sh", "-lc", "python run.py"})
		})
	}
}

func TestDetectReadmeScansLaterSections(t *testing.T) {
	md := `# Repo

## Install

Nothing here.

## Usage

` + "```sh" + `
node app.js
` + "```" + `
`
	p := mustDetect(t, map[string]string{"README.md": md})
	if !p.Runnable() {
		t.Fatalf("expected the Usage section to be used, got %q", p.NoDemoReason)
	}
	equalStrings(t, "demo", p.DemoCommand, []string{"sh", "-lc", "node app.js"})
}

// Fixture 4: READMEs whose demo section exists but has nothing positively
// runnable. Each must be a no_runnable_demo verdict, never an error.
func TestDetectReadmeNoRunnableBlock(t *testing.T) {
	cases := []struct {
		name     string
		readme   string
		contains string
	}{
		{
			name:     "heading only",
			readme:   "## Quick Start\n\nComing soon.\n",
			contains: "no fenced code block",
		},
		{
			name:     "maven xml",
			readme:   "## Quick Start\n\n```xml\n<dependency>\n  <groupId>org.example</groupId>\n</dependency>\n```\n",
			contains: "not a shell fence",
		},
		{
			name:     "library usage",
			readme:   "## Usage\n\n```python\nfrom lib import Client\nClient().run()\n```\n",
			contains: "not a shell fence",
		},
		{
			name:     "placeholder key",
			readme:   "## Quick start\n\n```bash\nexport KEY=<your-api-key>\npython demo.py\n```\n",
			contains: "placeholder",
		},
		{
			name:     "placeholder your token",
			readme:   "## Quick start\n\n```bash\nYOUR_API_KEY=abc python demo.py\n```\n",
			contains: "placeholder",
		},
		{
			name:     "placeholder brace",
			readme:   "## Quick start\n\n```bash\nexport API_KEY=${API_KEY}\npython demo.py\n```\n",
			contains: "placeholder",
		},
		{
			name:     "cuda prerequisite",
			readme:   "## Setup\n\n```bash\npython serve.py --cuda\n```\n",
			contains: "GPU",
		},
		{
			name:     "machine prerequisite",
			readme:   "## Install\n\n```bash\nsudo apt-get install -y ffmpeg\npython demo.py\n```\n",
			contains: "machine-level",
		},
		{
			name:     "install only",
			readme:   "## Install\n\n```bash\npip install -r requirements.txt\n```\n",
			contains: "only installs",
		},
		{
			name:     "no heading",
			readme:   "# Repo\n\n```bash\npython demo.py\n```\n",
			contains: "no quickstart-style heading",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := mustDetect(t, map[string]string{"README.md": tc.readme})
			if p.Runnable() {
				t.Fatalf("expected no_runnable_demo, got demo %#v", p.DemoCommand)
			}
			if !strings.Contains(p.NoDemoReason, tc.contains) {
				t.Fatalf("reason %q does not contain %q", p.NoDemoReason, tc.contains)
			}
		})
	}
}

// Fixture 5: a SKILL.md-only repo. Frontmatter parsing is tolerant and the
// verdict is still no_runnable_demo.
func TestDetectSkillOnly(t *testing.T) {
	p := mustDetect(t, map[string]string{
		"SKILL.md": `---
name: pdf-tools
description: Help with PDFs
version: 1.2.3
allowed-tools: Bash Read
hidden: false
metadata:
  category: documents
---

# PDF tools

Ask the model to summarize a PDF.
`,
	})
	if p.Runnable() {
		t.Fatalf("a prompt-only skill must not produce a demo, got %#v", p.DemoCommand)
	}
	if !strings.Contains(p.NoDemoReason, "SKILL.md") || !strings.Contains(p.NoDemoReason, "pdf-tools") {
		t.Fatalf("reason = %q", p.NoDemoReason)
	}
}

func TestDetectEmptyRepo(t *testing.T) {
	p := mustDetect(t, map[string]string{".keep": ""})
	if p.Runnable() || !strings.Contains(p.NoDemoReason, "no package.json") {
		t.Fatalf("reason = %q", p.NoDemoReason)
	}
}

func TestParseSkillFrontmatterTolerant(t *testing.T) {
	t.Run("no frontmatter", func(t *testing.T) {
		fm, found := detect.ParseSkillFrontmatter([]byte("# Skill\n\ntext\n"))
		if found || fm != nil {
			t.Fatalf("found=%v fm=%v", found, fm)
		}
	})
	t.Run("unknown keys ignored", func(t *testing.T) {
		doc := "---\nname: x\nallowed-tools: Bash\nhidden: true\nmetadata:\n  a: b\n---\nbody\n"
		fm, found := detect.ParseSkillFrontmatter([]byte(doc))
		if !found {
			t.Fatal("frontmatter not found")
		}
		if fm["name"] != "x" {
			t.Fatalf("name = %v", fm["name"])
		}
		if _, ok := fm["allowed-tools"]; !ok {
			t.Fatalf("allowed-tools missing from %v", fm)
		}
	})
	t.Run("malformed yaml tolerated", func(t *testing.T) {
		fm, found := detect.ParseSkillFrontmatter([]byte("---\nname: [unclosed\n---\n"))
		if found || fm != nil {
			t.Fatalf("malformed frontmatter must be treated as absent, got found=%v fm=%v", found, fm)
		}
	})
	t.Run("crlf", func(t *testing.T) {
		fm, found := detect.ParseSkillFrontmatter([]byte("---\r\nname: y\r\n---\r\n"))
		if !found || fm["name"] != "y" {
			t.Fatalf("found=%v fm=%v", found, fm)
		}
	})
}

func TestDetectPrefersManifestOverReadme(t *testing.T) {
	p := mustDetect(t, map[string]string{
		"package.json": `{"scripts":{"demo":"node index.js"}}`,
		"README.md":    "## Quick Start\n\n```bash\npython other.py\n```\n",
	})
	equalStrings(t, "demo", p.DemoCommand, []string{"npm", "run", "demo"})
	if p.Evidence != "package.json:scripts.demo" {
		t.Fatalf("evidence = %q", p.Evidence)
	}
}

func TestDetectMergesReadmeInstallIntoManifest(t *testing.T) {
	p := mustDetect(t, map[string]string{
		"requirements.txt": "flask\n",
		"README.md":        "## Quick start\n\n```bash\npip install requests\npython app.py\n```\n",
	})
	equalStrings(t, "install", p.Install, []string{
		"pip install -r requirements.txt",
		"pip install requests",
	})
	equalStrings(t, "demo", p.DemoCommand, []string{"sh", "-lc", "python app.py"})
}
