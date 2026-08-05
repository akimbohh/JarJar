package provision

import (
	"strings"
	"testing"
)

func TestMergeServerProperties_PreservesAndSets(t *testing.T) {
	existing := "# a comment\n" +
		"level-name=world\n" +
		"server-port=12345\n" + // should be overwritten
		"difficulty=hard\n" +
		"\n" +
		"# trailing comment\n"

	updates := managedProperties(25565, 25575, "SECRETpass123456")
	out := mergeServerProperties(existing, updates)

	props := parseProps(out)

	// Existing unrelated keys preserved.
	if props["level-name"] != "world" {
		t.Errorf("level-name not preserved: got %q", props["level-name"])
	}
	if props["difficulty"] != "hard" {
		t.Errorf("difficulty not preserved: got %q", props["difficulty"])
	}
	// Comments preserved.
	if !strings.Contains(out, "# a comment") || !strings.Contains(out, "# trailing comment") {
		t.Errorf("comments not preserved:\n%s", out)
	}
	// Existing managed key overwritten in place, not duplicated.
	if props["server-port"] != "25565" {
		t.Errorf("server-port not overwritten: got %q", props["server-port"])
	}
	if strings.Count(out, "server-port=") != 1 {
		t.Errorf("server-port duplicated:\n%s", out)
	}
	// RCON keys set.
	if props["enable-rcon"] != "true" {
		t.Errorf("enable-rcon = %q, want true", props["enable-rcon"])
	}
	if props["rcon.port"] != "25575" {
		t.Errorf("rcon.port = %q, want 25575", props["rcon.port"])
	}
	if props["rcon.password"] != "SECRETpass123456" {
		t.Errorf("rcon.password = %q, want the generated password", props["rcon.password"])
	}
	if props["online-mode"] != "true" || props["spawn-protection"] != "0" || props["motd"] != "A JarJar server" {
		t.Errorf("managed keys not all set: %#v", props)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output should end with newline")
	}
}

func TestMergeServerProperties_EmptyStart(t *testing.T) {
	out := mergeServerProperties("", managedProperties(25565, 25575, "pw"))
	props := parseProps(out)
	for _, k := range []string{"enable-rcon", "rcon.password", "rcon.port", "server-port", "motd", "online-mode", "spawn-protection"} {
		if _, ok := props[k]; !ok {
			t.Errorf("key %q missing when starting from empty file", k)
		}
	}
}

func TestMergeServerProperties_Idempotent(t *testing.T) {
	updates := managedProperties(25565, 25575, "pw")
	once := mergeServerProperties("", updates)
	twice := mergeServerProperties(once, updates)
	if once != twice {
		t.Errorf("merge not idempotent:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
}

func TestGenerateRconPassword(t *testing.T) {
	const n = 16
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		pw, err := generateRconPassword(n)
		if err != nil {
			t.Fatalf("generateRconPassword: %v", err)
		}
		if len(pw) != n {
			t.Fatalf("length = %d, want %d", len(pw), n)
		}
		for _, c := range pw {
			if !strings.ContainsRune(rconPasswordCharset, c) {
				t.Fatalf("password %q contains non-alphanumeric %q", pw, c)
			}
		}
		seen[pw] = true
	}
	// 200 draws from a 62^16 space should never collide.
	if len(seen) != 200 {
		t.Errorf("expected 200 unique passwords, got %d", len(seen))
	}
}

func TestBuildSystemdUnit_UserOmittedWhenEmpty(t *testing.T) {
	unit := buildSystemdUnit("/srv/mc", "/srv/mc/jarjar-start.sh", "")
	if strings.Contains(unit, "User=") {
		t.Errorf("User= should be omitted when serviceUser empty:\n%s", unit)
	}
	if !strings.Contains(unit, "ExecStart=/srv/mc/jarjar-start.sh") {
		t.Errorf("ExecStart missing:\n%s", unit)
	}
	if !strings.Contains(unit, "WorkingDirectory=/srv/mc") {
		t.Errorf("WorkingDirectory missing:\n%s", unit)
	}
	if !strings.Contains(unit, "Type=simple") || !strings.Contains(unit, "Restart=always") || !strings.Contains(unit, "TimeoutStopSec=120") {
		t.Errorf("required service directives missing:\n%s", unit)
	}
}

func TestBuildSystemdUnit_UserIncludedWhenSet(t *testing.T) {
	unit := buildSystemdUnit("/srv/mc", "/srv/mc/jarjar-start.sh", "minecraft")
	if !strings.Contains(unit, "User=minecraft") {
		t.Errorf("User=minecraft should be present:\n%s", unit)
	}
}

func TestRconPortFrom(t *testing.T) {
	cases := []struct {
		addr    string
		want    int
		wantErr bool
	}{
		{"127.0.0.1:25575", 25575, false},
		{"0.0.0.0:25580", 25580, false},
		{"[::1]:25575", 25575, false},
		{"nonsense", 0, true},
		{"127.0.0.1:abc", 0, true},
	}
	for _, c := range cases {
		got, err := rconPortFrom(c.addr)
		if c.wantErr {
			if err == nil {
				t.Errorf("rconPortFrom(%q): expected error", c.addr)
			}
			continue
		}
		if err != nil {
			t.Errorf("rconPortFrom(%q): unexpected error %v", c.addr, err)
			continue
		}
		if got != c.want {
			t.Errorf("rconPortFrom(%q) = %d, want %d", c.addr, got, c.want)
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	var o Options
	o.applyDefaults()
	if o.JavaPath != "java" {
		t.Errorf("JavaPath default = %q, want java", o.JavaPath)
	}
	if o.MemoryMB != 9216 {
		t.Errorf("MemoryMB default = %d, want 9216", o.MemoryMB)
	}
	if o.ServerPort != 25565 {
		t.Errorf("ServerPort default = %d, want 25565", o.ServerPort)
	}
	if o.RconAddr != "127.0.0.1:25575" {
		t.Errorf("RconAddr default = %q, want 127.0.0.1:25575", o.RconAddr)
	}
	if o.Log == nil {
		t.Errorf("Log should default to slog.Default()")
	}
}

func TestLaunchLine_FabricAndForgeLikes(t *testing.T) {
	fab := launchLine(Options{Loader: Loader{ID: "fabric"}, JavaPath: "java", MemoryMB: 4096})
	if !strings.Contains(fab, "-jar fabric-server-launch.jar nogui") {
		t.Errorf("fabric launch line wrong: %q", fab)
	}
	if !strings.Contains(fab, "-Xmx4096M") || !strings.Contains(fab, "-Xms4096M") {
		t.Errorf("fabric launch line missing heap flags: %q", fab)
	}

	// Forge without a run.sh on disk falls back to the @args file.
	forge := launchLine(Options{Loader: Loader{ID: "forge", Version: "47.2.0"}, MCVersion: "1.20.1", JavaPath: "java", MemoryMB: 4096, ServerDir: "/does/not/exist"})
	if !strings.Contains(forge, "@libraries/net/minecraftforge/forge/1.20.1-47.2.0/unix_args.txt nogui") {
		t.Errorf("forge fallback launch line wrong: %q", forge)
	}
}

// parseProps parses server.properties text into a key/value map (ignoring
// comments and blank lines), for assertions.
func parseProps(s string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || !strings.Contains(line, "=") {
			continue
		}
		i := strings.Index(line, "=")
		m[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
	}
	return m
}
