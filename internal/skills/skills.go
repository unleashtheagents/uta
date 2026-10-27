// Package skills implements unified customization discovery inspired by
// Google Antigravity (agy) and Anthropic Claude Code (claude).
//
// It discovers hierarchical project rules (AGENTS.md, CLAUDE.md, GEMINI.md,
// .uta/rules/*.md, .agents/rules/*.md), progressive on-demand skills
// (skills/<name>/SKILL.md across .uta/, .agents/, .claude/, and global homes),
// subagent definitions (.claude/agents/*.md, .uta/agents/*.md), and lifecycle
// hooks (.uta/hooks.json, .agents/hooks.json).
//
// It also supports progressive disclosure context generation and safe @file
// context expansion so cloud-hosted models (such as NVIDIA Nemotron on Nebius
// Token Factory) receive the same rich workspace context as local agent CLIs.
package skills

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Kind identifies the customization category.
type Kind string

const (
	KindRule     Kind = "rule"
	KindSkill    Kind = "skill"
	KindSubagent Kind = "subagent"
	KindHook     Kind = "hook"
)

// Item represents a single discovered rule, skill, subagent, or hook.
type Item struct {
	Name        string `json:"name"`
	Kind        Kind   `json:"kind"`
	Ecosystem   string `json:"ecosystem"` // "uta", "agy", "claude", or "shared"
	Scope       string `json:"scope"`     // "workspace" or "global"
	Description string `json:"description"`
	Trigger     string `json:"trigger,omitempty"` // "always_on" or "on_demand"
	Path        string `json:"path"`
	Body        string `json:"-"`
}

// Catalog holds all deduplicated customizations discovered for a workdir.
type Catalog struct {
	Workdir string `json:"workdir"`
	Items   []Item `json:"items"`
}

// Rules returns all discovered rule items.
func (c *Catalog) Rules() []Item {
	return c.byKind(KindRule)
}

// Skills returns all discovered skill items.
func (c *Catalog) Skills() []Item {
	return c.byKind(KindSkill)
}

// Subagents returns all discovered subagent items.
func (c *Catalog) Subagents() []Item {
	return c.byKind(KindSubagent)
}

// Hooks returns all discovered hook items.
func (c *Catalog) Hooks() []Item {
	return c.byKind(KindHook)
}

func (c *Catalog) byKind(k Kind) []Item {
	var out []Item
	for _, it := range c.Items {
		if it.Kind == k {
			out = append(out, it)
		}
	}
	return out
}

// Find looks up an item by case-insensitive name or base filename.
func (c *Catalog) Find(query string) (Item, bool) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return Item{}, false
	}
	for _, it := range c.Items {
		if strings.ToLower(it.Name) == q || strings.ToLower(filepath.Base(it.Path)) == q {
			return it, true
		}
	}
	return Item{}, false
}

// Discover walks from workdir up to the repository root (.git or .uta) and
// optionally inspects global directories under homeDir, deduplicating by
// resolved file path and name precedence (workspace > global).
func Discover(workdir, homeDir string) *Catalog {
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	absWorkdir, err := filepath.Abs(workdir)
	if err == nil {
		workdir = absWorkdir
	}

	seenPaths := map[string]bool{}
	seenSkillNames := map[string]bool{}
	var items []Item

	addItem := func(it Item) {
		resolved := resolveRealPath(it.Path)
		if seenPaths[resolved] {
			return
		}
		if it.Kind == KindSkill {
			key := strings.ToLower(it.Name)
			if seenSkillNames[key] {
				return
			}
			seenSkillNames[key] = true
		}
		seenPaths[resolved] = true
		items = append(items, it)
	}

	// 1. Walk up from workdir to repo root for hierarchical rules & workspace skills.
	dirs := ancestorChainToRepoRoot(workdir)
	for _, dir := range dirs {
		// Hierarchical root rule files (AGENTS.md, CLAUDE.md, GEMINI.md)
		for _, ruleFile := range []struct {
			file string
			eco  string
		}{
			{"AGENTS.md", "agy"},
			{"CLAUDE.md", "claude"},
			{"GEMINI.md", "agy"},
		} {
			p := filepath.Join(dir, ruleFile.file)
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				raw, _ := os.ReadFile(p)
				name, desc, trigger, body := parseMarkdownWithFrontmatter(string(raw), ruleFile.file)
				if trigger == "" {
					trigger = "always_on"
				}
				addItem(Item{
					Name:        name,
					Kind:        KindRule,
					Ecosystem:   ruleFile.eco,
					Scope:       "workspace",
					Description: desc,
					Trigger:     trigger,
					Path:        p,
					Body:        body,
				})
			}
		}

		// Directory rules: .uta/rules/*.md, .agents/rules/*.md, .agent/rules/*.md
		for _, rdir := range []struct {
			sub string
			eco string
		}{
			{filepath.Join(".uta", "rules"), "uta"},
			{filepath.Join(".agents", "rules"), "agy"},
			{filepath.Join(".agent", "rules"), "agy"},
		} {
			scanMarkdownFiles(filepath.Join(dir, rdir.sub), KindRule, rdir.eco, "workspace", "always_on", addItem)
		}

		// Workspace skills: .uta/skills/*/SKILL.md, .agents/skills/*/SKILL.md, .claude/skills/*/SKILL.md
		for _, sdir := range []struct {
			sub string
			eco string
		}{
			{filepath.Join(".uta", "skills"), "uta"},
			{filepath.Join(".agents", "skills"), "agy"},
			{filepath.Join(".agent", "skills"), "agy"},
			{filepath.Join(".claude", "skills"), "claude"},
		} {
			scanSkillsDir(filepath.Join(dir, sdir.sub), sdir.eco, "workspace", addItem)
		}

		// Workspace subagents: .claude/agents/*.md, .uta/agents/*.md, .agents/agents/*.md
		for _, adir := range []struct {
			sub string
			eco string
		}{
			{filepath.Join(".claude", "agents"), "claude"},
			{filepath.Join(".uta", "agents"), "uta"},
			{filepath.Join(".agents", "agents"), "agy"},
		} {
			scanMarkdownFiles(filepath.Join(dir, adir.sub), KindSubagent, adir.eco, "workspace", "on_demand", addItem)
		}

		// Workspace hooks: .uta/hooks.json, .agents/hooks.json
		for _, hfile := range []struct {
			sub string
			eco string
		}{
			{filepath.Join(".uta", "hooks.json"), "uta"},
			{filepath.Join(".agents", "hooks.json"), "agy"},
		} {
			hp := filepath.Join(dir, hfile.sub)
			if info, err := os.Stat(hp); err == nil && !info.IsDir() {
				raw, _ := os.ReadFile(hp)
				addItem(Item{
					Name:        filepath.Base(hp),
					Kind:        KindHook,
					Ecosystem:   hfile.eco,
					Scope:       "workspace",
					Description: "Lifecycle event hooks configuration",
					Trigger:     "lifecycle",
					Path:        hp,
					Body:        string(raw),
				})
			}
		}
	}

	// 2. Global customizations under homeDir
	if homeDir != "" {
		for _, gdir := range []struct {
			sub string
			eco string
		}{
			{filepath.Join(homeDir, ".uta", "skills"), "uta"},
			{filepath.Join(homeDir, ".gemini", "config", "skills"), "agy"},
			{filepath.Join(homeDir, ".claude", "skills"), "claude"},
		} {
			scanSkillsDir(gdir.sub, gdir.eco, "global", addItem)
		}
	}

	return &Catalog{
		Workdir: workdir,
		Items:   items,
	}
}

// BuildProgressiveContext constructs a compact system prompt section combining
// always_on workspace rules with progressive disclosure summaries (name +
// description only) for discovered skills and subagents.
func (c *Catalog) BuildProgressiveContext(maxRuleBytes int) string {
	if c == nil || len(c.Items) == 0 {
		return ""
	}
	if maxRuleBytes <= 0 {
		maxRuleBytes = 4096
	}

	var b strings.Builder
	rules := c.Rules()
	if len(rules) > 0 {
		b.WriteString("<workspace_rules>\n")
		used := 0
		for _, r := range rules {
			if r.Trigger != "" && r.Trigger != "always_on" {
				continue
			}
			txt := strings.TrimSpace(r.Body)
			if txt == "" {
				continue
			}
			if used+len(txt) > maxRuleBytes {
				rem := maxRuleBytes - used
				if rem > 128 {
					txt = txt[:rem] + "\n...[truncated]"
				} else {
					break
				}
			}
			b.WriteString("## " + r.Name + " (" + r.Ecosystem + ")\n")
			b.WriteString(txt + "\n\n")
			used += len(txt)
		}
		b.WriteString("</workspace_rules>\n")
	}

	skills := c.Skills()
	if len(skills) > 0 {
		b.WriteString("<available_skills>\n")
		for _, s := range skills {
			b.WriteString("- " + s.Name + " [" + s.Ecosystem + "]: " + s.Description + "\n")
		}
		b.WriteString("</available_skills>\n")
	}

	subagents := c.Subagents()
	if len(subagents) > 0 {
		b.WriteString("<available_subagents>\n")
		for _, sa := range subagents {
			b.WriteString("- " + sa.Name + " [" + sa.Ecosystem + "]: " + sa.Description + "\n")
		}
		b.WriteString("</available_subagents>\n")
	}

	return strings.TrimSpace(b.String())
}

var atMentionRE = regexp.MustCompile(`(?:^|\s)@([A-Za-z0-9_./-]+)`)

var reservedAgentNames = map[string]bool{
	"nebius": true,
	"claude": true,
	"agy":    true,
	"gemini": true,
	"codex":  true,
	"ultra":  true,
	"super":  true,
}

// ExpandFileMentions scans prompt for @path/to/file references (as used in
// Google Antigravity and Claude Code). When a token matches a regular file
// inside workdir, its contents (bounded to 16 KiB per file) are appended in a
// structured <attached_files> block so cloud inference providers like Nebius
// Nemotron can inspect local workspace files directly.
func ExpandFileMentions(prompt, workdir string) (expanded string, attached []string) {
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	rootReal := resolveRealPath(workdir)
	if rootReal == "" {
		return prompt, nil
	}

	matches := atMentionRE.FindAllStringSubmatch(prompt, -1)
	if len(matches) == 0 {
		return prompt, nil
	}

	seen := map[string]bool{}
	var blocks []string

	for _, m := range matches {
		ref := strings.TrimRight(m[1], ".,;:!?)'\"")
		if ref == "" || reservedAgentNames[strings.ToLower(ref)] {
			continue
		}
		if seen[ref] {
			continue
		}
		candidate := filepath.Join(rootReal, filepath.FromSlash(ref))
		candReal := resolveRealPath(candidate)
		if !isWithinDir(candReal, rootReal) {
			continue
		}
		info, err := os.Stat(candReal)
		if err != nil || info.IsDir() || !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(candReal)
		if err != nil {
			continue
		}
		const maxFileBytes = 16 * 1024
		content := string(raw)
		if len(content) > maxFileBytes {
			content = content[:maxFileBytes] + "\n...[truncated]"
		}
		seen[ref] = true
		attached = append(attached, ref)
		blocks = append(blocks, "<file path=\""+ref+"\">\n"+strings.TrimSpace(content)+"\n</file>")
	}

	if len(blocks) == 0 {
		return prompt, nil
	}
	return prompt + "\n\n<attached_files>\n" + strings.Join(blocks, "\n\n") + "\n</attached_files>", attached
}

func isWithinDir(target, root string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}

func ancestorChainToRepoRoot(start string) []string {
	var dirs []string
	cur := start
	for i := 0; i < 16 && cur != ""; i++ {
		dirs = append(dirs, cur)
		if isRepoBoundary(cur) {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return dirs
}

func isRepoBoundary(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, ".uta")); err == nil {
		return true
	}
	return false
}

func scanSkillsDir(dir, eco, scope string, add func(Item)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, dname := range names {
		skillFile := filepath.Join(dir, dname, "SKILL.md")
		info, err := os.Stat(skillFile)
		if err != nil || info.IsDir() {
			continue
		}
		raw, err := os.ReadFile(skillFile)
		if err != nil {
			continue
		}
		name, desc, trigger, body := parseMarkdownWithFrontmatter(string(raw), dname)
		if trigger == "" {
			trigger = "on_demand"
		}
		add(Item{
			Name:        name,
			Kind:        KindSkill,
			Ecosystem:   eco,
			Scope:       scope,
			Description: desc,
			Trigger:     trigger,
			Path:        skillFile,
			Body:        body,
		})
	}
}

func scanMarkdownFiles(dir string, kind Kind, eco, scope, defaultTrigger string, add func(Item)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, fname := range files {
		p := filepath.Join(dir, fname)
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		fallbackName := strings.TrimSuffix(fname, filepath.Ext(fname))
		name, desc, trigger, body := parseMarkdownWithFrontmatter(string(raw), fallbackName)
		if trigger == "" {
			trigger = defaultTrigger
		}
		add(Item{
			Name:        name,
			Kind:        kind,
			Ecosystem:   eco,
			Scope:       scope,
			Description: desc,
			Trigger:     trigger,
			Path:        p,
			Body:        body,
		})
	}
}

func parseMarkdownWithFrontmatter(raw, fallbackName string) (name, desc, trigger, body string) {
	name = fallbackName
	body = raw
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "---\n") || strings.HasPrefix(trimmed, "---\r\n") {
		rest := trimmed[4:]
		if idx := strings.Index(rest, "\n---"); idx >= 0 {
			fm := rest[:idx]
			body = strings.TrimSpace(rest[idx+4:])
			for _, line := range strings.Split(fm, "\n") {
				line = strings.TrimSpace(line)
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				k = strings.ToLower(strings.TrimSpace(k))
				v = strings.Trim(strings.TrimSpace(v), `"'`)
				if v == ">-" || v == "|" || v == ">" {
					continue
				}
				switch k {
				case "name":
					if v != "" {
						name = v
					}
				case "description":
					if v != "" {
						desc = v
					}
				case "trigger":
					if v != "" {
						trigger = v
					}
				}
			}
		}
	}
	if desc == "" {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "<!--") {
				continue
			}
			desc = line
			if len(desc) > 120 {
				desc = desc[:117] + "..."
			}
			break
		}
	}
	if desc == "" {
		desc = fallbackName
	}
	return name, desc, trigger, body
}

func resolveRealPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}
