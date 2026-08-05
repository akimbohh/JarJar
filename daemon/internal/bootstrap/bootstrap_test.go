package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/akimbohh/jarjar/daemon/internal/config"
)

func TestGenesisPlanValidate(t *testing.T) {
	good := GenesisPlan{
		MCVersion: "1.21.1",
		Loader:    GenesisLoader{ID: "fabric", Version: "0.16.0"},
		Source:    GenesisSource{Type: "scratch", Mods: []GenesisMod{{Platform: "modrinth", ProjectID: "AABBCC", ProjectSlug: "sodium", VersionID: "v1"}}},
		PackName:  "Test Pack",
	}
	if err := good.validate(); err != nil {
		t.Fatalf("valid scratch plan rejected: %v", err)
	}

	existing := GenesisPlan{
		MCVersion: "1.21.1",
		Loader:    GenesisLoader{ID: "neoforge", Version: "21.1.0"},
		Source:    GenesisSource{Type: "existing_pack", Platform: "modrinth", ProjectID: "P1", VersionID: "V1"},
		PackName:  "ATM",
	}
	if err := existing.validate(); err != nil {
		t.Fatalf("valid existing_pack plan rejected: %v", err)
	}

	bad := []GenesisPlan{
		{MCVersion: "", Loader: GenesisLoader{ID: "fabric"}, Source: existing.Source, PackName: "x"},
		{MCVersion: "1.21", Loader: GenesisLoader{ID: "bogus"}, Source: existing.Source, PackName: "x"},
		{MCVersion: "1.21", Loader: GenesisLoader{ID: "fabric"}, Source: GenesisSource{Type: "scratch"}, PackName: "x"},
		{MCVersion: "1.21", Loader: GenesisLoader{ID: "fabric"}, Source: GenesisSource{Type: "existing_pack", Platform: "modrinth"}, PackName: "x"},
		{MCVersion: "1.21", Loader: GenesisLoader{ID: "fabric"}, Source: existing.Source, PackName: ""},
	}
	for i, p := range bad {
		if err := p.validate(); err == nil {
			t.Errorf("bad plan #%d passed validation", i)
		}
	}
}

func TestWriteConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "jarjard.toml")

	cfg := config.Default()
	cfg.Server.DataDir = "/var/lib/jarjar"
	cfg.Minecraft.ServerDir = "/opt/minecraft"
	cfg.Minecraft.Control = "systemd"
	cfg.Minecraft.SystemdUnit = "minecraft.service"
	cfg.Minecraft.RconAddr = "127.0.0.1:25575"
	cfg.Minecraft.RconPassword = "SECRETpw"

	if err := writeConfig(path, cfg); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	// Written with owner-only perms (contains the rcon password).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config perms = %v, want 0600", fi.Mode().Perm())
	}

	var got config.Config
	if _, err := toml.DecodeFile(path, &got); err != nil {
		t.Fatalf("decode written config: %v", err)
	}
	if got.Minecraft.RconPassword != "SECRETpw" || got.Minecraft.ServerDir != "/opt/minecraft" {
		t.Errorf("round-trip mismatch: %+v", got.Minecraft)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("written config fails validation: %v", err)
	}
}

func TestManagedServerPath(t *testing.T) {
	yes := []string{"mods/x.jar", "config/a.toml", "kubejs/s.js", "datapacks/d.zip"}
	no := []string{"server.properties", "eula.txt", "world/level.dat", "libraries/x"}
	for _, p := range yes {
		if !managedServerPath(p) {
			t.Errorf("%q should be a managed server path", p)
		}
	}
	for _, p := range no {
		if managedServerPath(p) {
			t.Errorf("%q should NOT be a managed server path", p)
		}
	}
}
