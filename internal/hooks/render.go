package hooks

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// RenderClaudeSettings overwrites path with the role's Claude settings template
// carrying the hooks expected for the override key (e.g. "myrig/reviewer").
// Nothing already in the file survives, so it suits a role whose own session
// could have edited it.
func RenderClaudeSettings(path, key, role string) error {
	content, err := resolveAndSubstitute("claude", "settings.json", role)
	if err != nil {
		return err
	}
	settings, err := UnmarshalSettings(content)
	if err != nil {
		return fmt.Errorf("parsing settings template: %w", err)
	}
	expected, err := ComputeExpected(key)
	if err != nil {
		return fmt.Errorf("computing hooks for %s: %w", key, err)
	}
	settings.Hooks = *expected
	data, err := MarshalSettings(settings)
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating settings directory: %w", err)
	}
	return atomicfile.WriteFile(path, append(data, '\n'), 0o600)
}
