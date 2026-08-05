package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// httpTimeout bounds every download / metadata fetch. Loader installers pull the
// full Minecraft server jar and libraries, so this is generous.
const httpTimeout = 10 * time.Minute

// httpClient is the shared client for all provision downloads.
var httpClient = &http.Client{Timeout: httpTimeout}

// installLoader dispatches to the loader-specific installer. Each installer
// downloads its installer jar to a temp file, then runs it with the configured
// java, writing the server files into o.ServerDir.
func installLoader(ctx context.Context, o Options) error {
	switch strings.ToLower(o.Loader.ID) {
	case "fabric":
		return installFabric(ctx, o)
	case "quilt":
		return installQuilt(ctx, o)
	case "neoforge":
		return installNeoForge(ctx, o)
	case "forge":
		return installForge(ctx, o)
	default:
		return fmt.Errorf("provision: unknown loader %q (want fabric|neoforge|forge|quilt)", o.Loader.ID)
	}
}

// ---- fabric ----

// installFabric fetches the latest Fabric installer jar and runs it in server
// mode. The installer downloads the Minecraft server jar itself and writes
// fabric-server-launch.jar into ServerDir.
func installFabric(ctx context.Context, o Options) error {
	iv, err := latestFabricInstaller(ctx)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://maven.fabricmc.net/net/fabricmc/fabric-installer/%s/fabric-installer-%s.jar", iv, iv)
	jar, cleanup, err := downloadTemp(ctx, url, "fabric-installer-*.jar")
	if err != nil {
		return err
	}
	defer cleanup()

	// fabric-installer server -mcversion <mc> -loader <loaderver> -downloadMinecraft -dir <ServerDir>
	return runJava(ctx, o, "-jar", jar, "server",
		"-mcversion", o.MCVersion,
		"-loader", o.Loader.Version,
		"-downloadMinecraft",
		"-dir", o.ServerDir,
	)
}

// latestFabricInstaller returns the version of the newest stable Fabric
// installer (falling back to the first entry when none is flagged stable).
func latestFabricInstaller(ctx context.Context) (string, error) {
	var versions []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := getJSON(ctx, "https://meta.fabricmc.net/v2/versions/installer", &versions); err != nil {
		return "", fmt.Errorf("provision: fetch fabric installer versions: %w", err)
	}
	if len(versions) == 0 {
		return "", fmt.Errorf("provision: fabric meta returned no installer versions")
	}
	for _, v := range versions {
		if v.Stable {
			return v.Version, nil
		}
	}
	return versions[0].Version, nil
}

// ---- quilt ----
//
// TODO(quilt): best-effort. The Quilt installer CLI is less commonly exercised
// than Fabric's; the metadata endpoint, maven coordinates, and the exact
// `install server` flags below should be confirmed on-box against the installer
// version actually downloaded before relying on quilt in production. Fabric,
// neoforge, and forge are the priority loaders.
func installQuilt(ctx context.Context, o Options) error {
	iv, err := latestQuiltInstaller(ctx)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://maven.quiltmc.org/repository/release/org/quiltmc/quilt-installer/%s/quilt-installer-%s.jar", iv, iv)
	jar, cleanup, err := downloadTemp(ctx, url, "quilt-installer-*.jar")
	if err != nil {
		return err
	}
	defer cleanup()

	// quilt-installer install server <mc> <loaderver> --download-server --install-dir=<ServerDir>
	return runJava(ctx, o, "-jar", jar, "install", "server",
		o.MCVersion, o.Loader.Version,
		"--download-server",
		"--install-dir="+o.ServerDir,
	)
}

// latestQuiltInstaller returns the newest Quilt installer version. The v3 meta
// endpoint returns a JSON array of {version, maven}; there is no stable flag, so
// the first (newest) entry is used.
//
// TODO(quilt): confirm the response shape and ordering against the live endpoint.
func latestQuiltInstaller(ctx context.Context) (string, error) {
	var versions []struct {
		Version string `json:"version"`
	}
	if err := getJSON(ctx, "https://meta.quiltmc.org/v3/versions/installer", &versions); err != nil {
		return "", fmt.Errorf("provision: fetch quilt installer versions: %w", err)
	}
	if len(versions) == 0 {
		return "", fmt.Errorf("provision: quilt meta returned no installer versions")
	}
	return versions[0].Version, nil
}

// ---- neoforge ----

// installNeoForge downloads the NeoForge installer for the given loader version
// and installs the server. NeoForge does not embed the Minecraft version in the
// installer coordinate — the loader version alone selects it. After install
// NeoForge writes run.sh, user_jvm_args.txt, and a libraries/ tree.
func installNeoForge(ctx context.Context, o Options) error {
	url := fmt.Sprintf("https://maven.neoforged.net/releases/net/neoforged/neoforge/%s/neoforge-%s-installer.jar",
		o.Loader.Version, o.Loader.Version)
	jar, cleanup, err := downloadTemp(ctx, url, "neoforge-installer-*.jar")
	if err != nil {
		return err
	}
	defer cleanup()

	// neoforge installer: --installServer <ServerDir>
	return runJava(ctx, o, "-jar", jar, "--installServer", o.ServerDir)
}

// ---- forge ----

// installForge downloads the Forge installer (coordinate is <mc>-<loaderver>)
// and installs the server. Like NeoForge it writes run.sh / user_jvm_args.txt.
func installForge(ctx context.Context, o Options) error {
	combo := o.MCVersion + "-" + o.Loader.Version
	url := fmt.Sprintf("https://maven.minecraftforge.net/net/minecraftforge/forge/%s/forge-%s-installer.jar", combo, combo)
	jar, cleanup, err := downloadTemp(ctx, url, "forge-installer-*.jar")
	if err != nil {
		return err
	}
	defer cleanup()

	// forge installer: --installServer <ServerDir>
	return runJava(ctx, o, "-jar", jar, "--installServer", o.ServerDir)
}

// ---- shared helpers ----

// runJava runs the configured java with args, streaming output. The installer's
// combined stderr is folded into the returned error on failure.
func runJava(ctx context.Context, o Options, args ...string) error {
	cmd := exec.CommandContext(ctx, o.JavaPath, args...)
	cmd.Dir = o.ServerDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("provision: java %s: %w: %s", strings.Join(args, " "), err, tail(out, 800))
	}
	return nil
}

// getJSON GETs url and decodes the JSON body into dst.
func getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, tail(body, 200))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}

// downloadTemp streams url into a temp file matching pattern and returns its
// path plus a cleanup func the caller must defer.
func downloadTemp(ctx context.Context, url, pattern string) (path string, cleanup func(), err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", func() {}, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", func() {}, fmt.Errorf("provision: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", func() {}, fmt.Errorf("provision: download %s: HTTP %d: %s", url, resp.StatusCode, tail(body, 200))
	}

	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", func() {}, err
	}
	tmp := f.Name()
	cleanup = func() { os.Remove(tmp) }

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("provision: write %s: %w", filepath.Base(tmp), err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return tmp, cleanup, nil
}

// tail returns the trailing n bytes of b as a string (installer logs put the
// useful error at the end).
func tail(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}
