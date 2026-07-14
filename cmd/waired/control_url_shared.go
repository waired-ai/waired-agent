package main

import (
	"bufio"
	"os"
	"strings"
)

// parseControlURLFromEnvFile reads WAIRED_CONTROL_URL from a systemd-style
// KEY=VALUE env file — used on Linux (/etc/waired/agent.env) and macOS
// (<state-dir>/agent.env). Any read error is treated as "not configured"
// and returns "" — never fatal (the file is typically owner-only and may
// be unreadable to an unprivileged init).
func parseControlURLFromEnvFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "WAIRED_CONTROL_URL" {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if v != "" {
			return v
		}
	}
	return ""
}
