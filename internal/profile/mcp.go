package profile

import (
	"errors"
	"fmt"
	"strings"
)

// ProfileMCPServer describes one external MCP server the profile wants
// uta to launch and probe. The shape mirrors the canonical Anthropic
// .mcp.json schema (command + args + env), so a profile entry can be
// translated into a claude-compatible MCP config without rearrangement.
//
// Field semantics:
//
//   - Name:    identifier used in the "mcp__<name>__<tool>" tool pattern
//     claude understands in --allowedTools.
//   - Command: executable to launch (typically "npx", "uvx", a path).
//   - Args:    argv passed to Command.
//   - Env:     KEY=VALUE pairs added to the launched server's environment.
//     Values may reference parent env via the standard ${VAR}
//     syntax — expansion is the caller's responsibility.
type ProfileMCPServer struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
}

// Validate enforces the shape we depend on at bridge time: a non-empty
// name (which doubles as the "mcp__<name>__<tool>" namespace) and a
// non-empty command.
func (m *ProfileMCPServer) Validate() error {
	name := strings.TrimSpace(m.Name)
	if name == "" {
		return errors.New("name is required")
	}
	if name != m.Name {
		return errors.New("name must not contain leading or trailing whitespace")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("name %q must not contain path separators", name)
	}
	if strings.TrimSpace(m.Command) == "" {
		return fmt.Errorf("server %q: command is required", m.Name)
	}
	for k := range m.Env {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("server %q: env variable name must be non-empty", m.Name)
		}
		if strings.ContainsRune(k, '=') {
			return fmt.Errorf("server %q: env variable name %q must not contain '='", m.Name, k)
		}
	}
	return nil
}
