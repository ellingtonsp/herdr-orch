package daemon

import (
	"fmt"
	"strings"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// launchArg reads the last supplied flag value without interpreting agent-specific
// options. Flags after the agent's end-of-options marker are positional content.
func launchArg(args []string, flags ...string) string {
	value := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		for _, flag := range flags {
			if v, ok := strings.CutPrefix(arg, flag+"="); ok {
				value = v
			} else if arg == flag && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				value = args[i]
				break
			}
		}
	}
	return value
}

func dispatchWorkerMetadata(d store.Dispatch) string {
	parts := []string{}
	for _, field := range []struct{ label, value string }{
		{"agent", d.Agent}, {"account", d.Account}, {"profile", d.Profile}, {"requested model", d.Model},
	} {
		if field.value != "" {
			parts = append(parts, fmt.Sprintf("%s=%q", field.label, field.value))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "\nWorker launch: " + strings.Join(parts, ", ") + "."
}
