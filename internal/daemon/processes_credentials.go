package daemon

import (
	"fmt"
	"strconv"
	"strings"
)

type processCredentials struct {
	UIDs, GIDs           [4]uint64
	Permitted, Effective uint64
}

func parseProcessCredentials(status string) (processCredentials, error) {
	var c processCredentials
	seen := map[string]bool{}
	for _, line := range strings.Split(status, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "Uid:", "Gid:":
			if len(f) != 5 {
				return c, fmt.Errorf("invalid process credentials")
			}
			ids := &c.UIDs
			if f[0] == "Gid:" {
				ids = &c.GIDs
			}
			for i := 0; i < 4; i++ {
				v, err := strconv.ParseUint(f[i+1], 10, 32)
				if err != nil {
					return c, fmt.Errorf("invalid process credentials")
				}
				ids[i] = v
			}
			seen[f[0]] = true
		case "CapPrm:", "CapEff:":
			if len(f) != 2 {
				return c, fmt.Errorf("invalid process capabilities")
			}
			v, err := strconv.ParseUint(f[1], 16, 64)
			if err != nil {
				return c, fmt.Errorf("invalid process capabilities")
			}
			if f[0] == "CapPrm:" {
				c.Permitted = v
			} else {
				c.Effective = v
			}
			seen[f[0]] = true
		}
	}
	if len(seen) != 4 {
		return c, fmt.Errorf("incomplete process credentials")
	}
	return c, nil
}
func inspectableCredentials(target, caller processCredentials) bool {
	for _, id := range target.UIDs {
		if id != caller.UIDs[3] {
			return false
		}
	}
	for _, id := range target.GIDs {
		if id != caller.GIDs[3] {
			return false
		}
	}
	return target.Permitted & ^caller.Effective == 0
}
