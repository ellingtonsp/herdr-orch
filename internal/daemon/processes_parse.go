package daemon

import (
	"strconv"
	"strings"
)

func parseProcessTable(text string, uid int) []ProcessRecord {
	out := []ProcessRecord{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		owner, err := strconv.Atoi(f[2])
		if err != nil || owner != uid || strings.HasPrefix(f[8], "Z") {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		parent, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		out = append(out, ProcessRecord{UID: owner, UIDKnown: true, PID: pid, Parent: parent, Started: strings.Join(f[3:8], " "), Command: strings.Join(f[9:], " ")})
	}
	return out
}
func parseCwdTable(data []byte) map[int]string {
	out := map[int]string{}
	pid := 0
	for _, field := range strings.Split(string(data), "\x00") {
		field = strings.TrimPrefix(field, "\n")
		if len(field) == 0 {
			continue
		}
		switch field[0] {
		case 'p':
			pid, _ = strconv.Atoi(field[1:])
		case 'n':
			if pid > 0 && strings.HasPrefix(field[1:], "/") {
				out[pid] = processCwd(field[1:])
			}
		}
	}
	return out
}
