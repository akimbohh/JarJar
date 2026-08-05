// Package provision stands up a modded Minecraft *server* from nothing: given a
// Minecraft version, a mod loader, and a target directory it installs the loader
// server, accepts the EULA, writes a server.properties with RCON enabled, and
// emits a start script plus a systemd unit so the server can boot.
//
// Provision never starts the server and never writes to /etc — installing the
// unit and launching the process require root and are the caller's job. The
// heavy work (downloads, running the loader installer) shells out to the
// configured java, mirroring the exec/file patterns in internal/mcserver and
// internal/pack.
package provision

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Loader identifies a mod loader and its version.
type Loader struct {
	ID      string // "fabric" | "neoforge" | "forge" | "quilt"
	Version string // loader version
}

// Options configure a Provision run. Zero-valued fields take documented defaults.
type Options struct {
	MCVersion   string
	Loader      Loader
	ServerDir   string
	JavaPath    string // default "java" if empty
	MemoryMB    int    // JVM heap MB; default 9216
	ServerPort  int    // default 25565
	RconAddr    string // default "127.0.0.1:25575"
	ServiceUser string // systemd User=; if "" omit the User line
	Log         *slog.Logger
}

// Result reports how to run the provisioned server. The caller writes the
// systemd unit and starts the service.
type Result struct {
	RconPassword        string // randomly generated, also written into server.properties
	RconAddr            string
	SystemdUnitName     string // e.g. "minecraft.service"
	SystemdUnitContents string // full unit file text
	StartScript         string // absolute path to the generated start script
}

const (
	defaultJava       = "java"
	defaultMemoryMB   = 9216
	defaultServerPort = 25565
	defaultRconAddr   = "127.0.0.1:25575"

	userAgent = "jarjar/dev (github.com/akimbohh/JarJar)"

	systemdUnitName = "minecraft.service"
	startScriptName = "jarjar-start.sh"
)

// applyDefaults fills zero-valued options with their documented defaults.
func (o *Options) applyDefaults() {
	if o.JavaPath == "" {
		o.JavaPath = defaultJava
	}
	if o.MemoryMB == 0 {
		o.MemoryMB = defaultMemoryMB
	}
	if o.ServerPort == 0 {
		o.ServerPort = defaultServerPort
	}
	if o.RconAddr == "" {
		o.RconAddr = defaultRconAddr
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
}

// Provision installs and configures the server in o.ServerDir and returns how to
// run it. It is idempotent where reasonable (safe to re-run). It does NOT install
// the systemd unit or start the server — the caller does that (needs root).
func Provision(ctx context.Context, o Options) (Result, error) {
	o.applyDefaults()
	log := o.Log

	if o.ServerDir == "" {
		return Result{}, fmt.Errorf("provision: ServerDir is required")
	}
	if o.MCVersion == "" {
		return Result{}, fmt.Errorf("provision: MCVersion is required")
	}
	if o.Loader.ID == "" {
		return Result{}, fmt.Errorf("provision: Loader.ID is required")
	}

	// rcon.port is derived from RconAddr; fail early on a malformed address.
	rconPort, err := rconPortFrom(o.RconAddr)
	if err != nil {
		return Result{}, err
	}

	// 1. Java must be present before we bother downloading anything.
	if err := checkJava(ctx, o.JavaPath); err != nil {
		return Result{}, err
	}

	// 2. Target directory.
	if err := os.MkdirAll(o.ServerDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("provision: mkdir %s: %w", o.ServerDir, err)
	}

	// 3. Loader server install (network + java).
	if err := installLoader(ctx, o); err != nil {
		return Result{}, err
	}

	// 4. EULA.
	if err := writeEULA(o.ServerDir); err != nil {
		return Result{}, err
	}

	// 5. server.properties with RCON enabled.
	rconPassword, err := generateRconPassword(16)
	if err != nil {
		return Result{}, fmt.Errorf("provision: generate rcon password: %w", err)
	}
	if err := writeServerProperties(o.ServerDir, o.ServerPort, rconPort, rconPassword); err != nil {
		return Result{}, err
	}

	// 6. Start script (and, for forge-likes, user_jvm_args.txt).
	startScript, err := writeStartScript(o)
	if err != nil {
		return Result{}, err
	}

	// 7. systemd unit text (caller writes it under /etc).
	unit := buildSystemdUnit(o.ServerDir, startScript, o.ServiceUser)

	log.Info("provisioned minecraft server",
		"dir", o.ServerDir, "loader", o.Loader.ID, "loader_version", o.Loader.Version,
		"mc_version", o.MCVersion, "start_script", startScript)

	return Result{
		RconPassword:        rconPassword,
		RconAddr:            o.RconAddr,
		SystemdUnitName:     systemdUnitName,
		SystemdUnitContents: unit,
		StartScript:         startScript,
	}, nil
}

// checkJava runs `<java> -version` and returns a user-actionable error if java
// is missing or non-functional.
func checkJava(ctx context.Context, javaPath string) error {
	cmd := exec.CommandContext(ctx, javaPath, "-version")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Java not found — install a JRE (e.g. OpenJDK 21) and set claude/minecraft java path: %w: %s",
			err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// rconPortFrom extracts the port component of an addr like "127.0.0.1:25575".
func rconPortFrom(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("provision: rcon addr %q is not host:port: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("provision: rcon addr %q has non-numeric port: %w", addr, err)
	}
	return port, nil
}

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// serverPath joins the server dir with sub-elements.
func (o Options) serverPath(elem ...string) string {
	return filepath.Join(append([]string{o.ServerDir}, elem...)...)
}
