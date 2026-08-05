package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/store"
)

// serverManagedPrefixes are the only paths under server_dir the daemon ever
// writes or deletes (PIPELINE.md §6): the allowed content dirs plus mods/.
var serverManagedPrefixes = []string{"mods/", "config/", "defaultconfigs/", "kubejs/", "scripts/", "resourcepacks/", "shaderpacks/", "datapacks/"}

// serverApply syncs server-scoped files and server properties into the live
// server dir, restarts per policy, and health-checks the boot. On any failure
// it restores the backup and returns an error; the caller rolls back the pack
// repo build.
func (d Deps) serverApply(ctx context.Context, number int, m pack.Manifest, plan Plan) error {
	serverDir := d.Cfg.Minecraft.ServerDir
	backupDir := filepath.Join(d.Cfg.Server.DataDir, "rollback-tmp", itoa(number))

	// Target server file set (side server/both).
	target := map[string]string{} // path -> sha
	for _, f := range m.Files {
		if f.Side == pack.SideServer || f.Side == pack.SideBoth {
			if managedServerPath(f.Path) {
				target[f.Path] = f.SHA256
			}
		}
	}
	// Previous server file set.
	prev := map[string]string{}
	if number > 1 {
		if pm, err := d.Pack.LoadManifest(number - 1); err == nil {
			for _, f := range pm.Files {
				if (f.Side == pack.SideServer || f.Side == pack.SideBoth) && managedServerPath(f.Path) {
					prev[f.Path] = f.SHA256
				}
			}
		}
	}

	var toWrite, toDelete []string
	for path, sha := range target {
		if prev[path] != sha {
			toWrite = append(toWrite, path)
		}
	}
	for path := range prev {
		if _, ok := target[path]; !ok {
			toDelete = append(toDelete, path)
		}
	}
	sort.Strings(toWrite)
	sort.Strings(toDelete)

	// Restart window.
	if err := d.waitRestartWindow(ctx); err != nil {
		return err
	}

	d.MC.SetRestarting(true)
	d.emitServerStatus(ctx, "restarting")
	defer func() {
		d.MC.SetRestarting(false)
	}()

	// Stop the server (allow time to save).
	if err := d.MC.Stop(ctx); err != nil {
		d.Log.Warn("stop server", "err", err)
	}
	time.Sleep(2 * time.Second)

	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return err
	}

	applied, err := d.applyServerDelta(serverDir, backupDir, target, toWrite, toDelete, plan)
	if err != nil {
		d.restoreBackup(serverDir, backupDir)
		d.startAndIgnore(ctx)
		return err
	}

	// Start + health check.
	if err := d.MC.Start(ctx); err != nil {
		d.restoreBackup(serverDir, backupDir)
		d.startAndIgnore(ctx)
		return fmt.Errorf("start server: %w", err)
	}
	if err := d.MC.WaitHealthy(ctx); err != nil {
		d.Log.Error("boot health check failed; rolling back", "err", err)
		if rerr := d.MC.Stop(ctx); rerr != nil {
			d.Log.Warn("stop for rollback", "err", rerr)
		}
		d.restoreBackup(serverDir, backupDir)
		if serr := d.MC.Start(ctx); serr != nil {
			d.emitServerStatus(ctx, "down")
			return fmt.Errorf("boot failed and restore boot also failed: %v / %v", err, serr)
		}
		if herr := d.MC.WaitHealthy(ctx); herr != nil {
			d.emitServerStatus(ctx, "down")
			return fmt.Errorf("boot failed; restored server did not become healthy: %v", herr)
		}
		_ = applied
		return fmt.Errorf("new version failed to boot; rolled back to previous version")
	}

	// Success: clear backup.
	os.RemoveAll(backupDir)
	d.MC.SetRestarting(false)
	d.emitServerStatus(ctx, "running")
	return nil
}

// applyServerDelta backs up then writes/deletes files and applies server props.
func (d Deps) applyServerDelta(serverDir, backupDir string, target map[string]string, toWrite, toDelete []string, plan Plan) ([]string, error) {
	var applied []string

	backup := func(rel string) error {
		src := filepath.Join(serverDir, filepath.FromSlash(rel))
		if _, err := os.Stat(src); err != nil {
			return nil // nothing to back up
		}
		dst := filepath.Join(backupDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyFile(src, dst)
	}

	for _, rel := range toWrite {
		if err := backup(rel); err != nil {
			return applied, err
		}
		sha := target[rel]
		dst := filepath.Join(serverDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return applied, err
		}
		if err := copyFile(d.Pack.BlobPath(sha), dst+".jarjar-tmp"); err != nil {
			return applied, err
		}
		if err := os.Rename(dst+".jarjar-tmp", dst); err != nil {
			return applied, err
		}
		applied = append(applied, rel)
	}
	for _, rel := range toDelete {
		if err := backup(rel); err != nil {
			return applied, err
		}
		os.Remove(filepath.Join(serverDir, filepath.FromSlash(rel)))
		applied = append(applied, rel)
	}

	// server_property actions.
	var props []Action
	for _, a := range plan.Actions {
		if a.Type == ActServerProperty {
			props = append(props, a)
		}
	}
	if len(props) > 0 {
		if err := backup("server.properties"); err != nil {
			return applied, err
		}
		if err := applyServerProps(filepath.Join(serverDir, "server.properties"), props); err != nil {
			return applied, err
		}
	}
	return applied, nil
}

func (d Deps) waitRestartWindow(ctx context.Context) error {
	switch d.Cfg.Minecraft.RestartPolicy {
	case "immediate":
		d.MC.Say(ctx, "Server restarting for a modpack update in 60 seconds...")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Second):
		}
		d.MC.Say(ctx, "Restarting in 10 seconds...")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
		return nil
	default: // when_empty
		return d.MC.WaitEmpty(ctx, func(remaining time.Duration) {
			d.MC.Say(ctx, "A modpack update is queued and will apply when the server is empty.")
		})
	}
}

func (d Deps) restoreBackup(serverDir, backupDir string) {
	filepath.Walk(backupDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(backupDir, path)
		dst := filepath.Join(serverDir, rel)
		os.MkdirAll(filepath.Dir(dst), 0o755)
		copyFile(path, dst)
		return nil
	})
}

func (d Deps) startAndIgnore(ctx context.Context) {
	if err := d.MC.Start(ctx); err != nil {
		d.Log.Warn("restart after failure", "err", err)
	}
}

func (d Deps) emitServerStatus(ctx context.Context, state string) {
	buf := []byte(fmt.Sprintf(`{"state":%q}`, state))
	d.Store.AppendEvent(ctx, store.EventServerStatus, buf)
}

func managedServerPath(p string) bool {
	for _, pre := range serverManagedPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

func applyServerProps(path string, props []Action) error {
	buf, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	set := map[string]string{}
	for _, a := range props {
		set[a.Key] = a.Value
	}
	lines := strings.Split(string(buf), "\n")
	seen := map[string]bool{}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if eq := strings.Index(trimmed, "="); eq >= 0 {
			key := strings.TrimSpace(trimmed[:eq])
			if v, ok := set[key]; ok {
				lines[i] = key + "=" + v
				seen[key] = true
			}
		}
	}
	for k, v := range set {
		if !seen[k] {
			lines = append(lines, k+"="+v)
		}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
