package sandbox

import (
	"errors"
	"regexp"
)

// modelName admits a model identifier or alias, with Claude Code's
// 1M-context suffix (`sonnet[1m]`).
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}(\[1m\])?$`)

func ValidateAgent(provider, model string) error {
	if provider != "" && provider != "codex" && provider != "claude" {
		return errors.New("unknown agent provider")
	}
	if model != "" && !modelName.MatchString(model) {
		return errors.New("invalid model identifier")
	}
	return nil
}
