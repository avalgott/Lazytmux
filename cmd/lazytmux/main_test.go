package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPlanFlagHelpNamesUserConfigDir guards the plan flag help against
// re-pinning the plan path to ~/.config: plan.Load resolves it through
// os.UserConfigDir, which honors XDG_CONFIG_HOME, so a hardcoded default
// path would describe a location that is not always read.
func TestPlanFlagHelpNamesUserConfigDir(t *testing.T) {
	assert.Contains(t, planFlagHelp, "user config directory")
	assert.NotContains(t, planFlagHelp, "~/.config")
}
