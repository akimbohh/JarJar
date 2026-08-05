package provision

import (
	"crypto/rand"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// rconPasswordCharset is the alphabet for generated RCON passwords (alphanumeric
// so it survives quoting in server.properties and shells).
const rconPasswordCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// generateRconPassword returns an n-character alphanumeric password drawn from
// crypto/rand.
func generateRconPassword(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("password length must be positive")
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = rconPasswordCharset[int(b)%len(rconPasswordCharset)]
	}
	return string(out), nil
}

// writeEULA writes eula.txt accepting the Minecraft EULA (idempotent overwrite).
func writeEULA(serverDir string) error {
	path := serverDir + string(os.PathSeparator) + "eula.txt"
	if err := os.WriteFile(path, []byte("eula=true\n"), 0o644); err != nil {
		return fmt.Errorf("provision: write eula.txt: %w", err)
	}
	return nil
}

// property is one ordered key/value we manage in server.properties.
type property struct{ key, val string }

// writeServerProperties merges JarJar's managed keys (RCON, ports, motd, …) into
// any existing server.properties, then writes it back. Existing unrelated keys
// and comments are preserved and order is stable.
func writeServerProperties(serverDir string, serverPort, rconPort int, rconPassword string) error {
	path := serverDir + string(os.PathSeparator) + "server.properties"

	existing := ""
	if b, err := os.ReadFile(path); err == nil {
		existing = string(b)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("provision: read server.properties: %w", err)
	}

	// Forced keys (RCON/ports/identity) are always set; performance defaults are
	// only filled in when absent, so an operator's later tuning survives a
	// re-provision.
	merged := mergeServerProperties(existing, managedProperties(serverPort, rconPort, rconPassword))
	merged = fillMissingProperties(merged, performanceDefaults())
	if err := os.WriteFile(path, []byte(merged), 0o644); err != nil {
		return fmt.Errorf("provision: write server.properties: %w", err)
	}
	return nil
}

// managedProperties is the ordered set of server.properties keys JarJar sets.
func managedProperties(serverPort, rconPort int, rconPassword string) []property {
	return []property{
		{"enable-rcon", "true"},
		{"rcon.password", rconPassword},
		{"rcon.port", strconv.Itoa(rconPort)},
		{"server-port", strconv.Itoa(serverPort)},
		{"motd", "A JarJar server"},
		{"online-mode", "true"},
		{"spawn-protection", "0"},
	}
}

// performanceDefaults are sensible defaults for a smooth modded server. They are
// applied only when the key is not already present, so they set the baseline
// without overriding an operator's choices on a re-provision.
func performanceDefaults() []property {
	return []property{
		{"view-distance", "8"},
		{"simulation-distance", "6"},
		{"entity-broadcast-range-percentage", "80"},
		{"network-compression-threshold", "256"},
		{"sync-chunk-writes", "false"},
		{"max-tick-time", "-1"}, // disable the watchdog: modded worldgen can stall a tick
		{"enable-command-block", "true"},
	}
}

// fillMissingProperties appends each default whose key is not already a live
// "key=" line in content. Order of existing content is preserved; new keys are
// appended in the given order. Output ends with a trailing newline.
func fillMissingProperties(content string, defaults []property) string {
	present := map[string]bool{}
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || !strings.Contains(line, "=") {
			continue
		}
		key := strings.TrimSpace(line[:strings.Index(line, "=")])
		present[key] = true
	}
	out := lines
	for _, d := range defaults {
		if !present[d.key] {
			out = append(out, d.key+"="+d.val)
			present[d.key] = true
		}
	}
	return strings.Join(out, "\n") + "\n"
}

// mergeServerProperties applies the ordered updates onto existing content.
// Keys already present (even commented-out with a leading "#key=" are NOT
// treated as present — only live "key=" lines) are updated in place; keys not
// found are appended in update order. Blank lines, comments, and unmanaged keys
// are preserved verbatim. Output always ends with a trailing newline.
func mergeServerProperties(existing string, updates []property) string {
	// Fast lookup + applied tracking for the managed keys.
	want := make(map[string]string, len(updates))
	applied := make(map[string]bool, len(updates))
	for _, u := range updates {
		want[u.key] = u.val
	}

	var out []string
	if existing != "" {
		lines := strings.Split(strings.ReplaceAll(existing, "\r\n", "\n"), "\n")
		// Drop a single trailing empty element from a terminating newline so we
		// don't accumulate blank lines on repeated merges.
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		for _, line := range lines {
			trimmed := strings.TrimLeft(line, " \t")
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || !strings.Contains(line, "=") {
				out = append(out, line)
				continue
			}
			key := strings.TrimSpace(line[:strings.Index(line, "=")])
			if val, ok := want[key]; ok && !applied[key] {
				out = append(out, key+"="+val)
				applied[key] = true
				continue
			}
			out = append(out, line)
		}
	}

	// Append any managed keys that weren't already present, in order.
	for _, u := range updates {
		if !applied[u.key] {
			out = append(out, u.key+"="+u.val)
			applied[u.key] = true
		}
	}

	return strings.Join(out, "\n") + "\n"
}
