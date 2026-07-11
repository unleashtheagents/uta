package cli

import "errors"

// errNoProviders is the shared, actionable "nothing to run on" error.
// Every command that fans work out to a provider (run, audit, shadow,
// improve) funnels through this message so the fix is spelled out once,
// consistently: what to install, how to verify, where the tour is.
var errNoProviders = errors.New(`no provider is installed on PATH.

Install at least one provider CLI:
  claude   npm install -g @anthropic-ai/claude-code
  gemini   npm install -g @google/gemini-cli

Then verify the setup:   uta doctor
And take the 60s tour:   uta hello`)
