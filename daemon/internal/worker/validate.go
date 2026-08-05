package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/worker/modtool"
	"gopkg.in/yaml.v3"
)

// allowedEditPrefixes are the only paths the model may create or edit (V2).
var allowedEditPrefixes = []string{"config/", "defaultconfigs/", "kubejs/", "scripts/"}

// serverPropBlocklist are server properties the model must not change (V8).
var serverPropBlocklist = map[string]bool{"level-name": true}

type resolvedMod struct {
	Platform    string
	ProjectSlug string
	Side        string
	Version     modtool.Version
	AutoAdded   bool
}

type validateResult struct {
	Plan        Plan
	Adds        []resolvedMod // add_mod + update_mod + auto-added dependencies
	Removes     []string      // paths removed (remove_mod + update_mod.from_path)
	ServerProps []Action
}

// validate runs V1–V8 (V9 approval gate is handled by the caller).
func (d Deps) validate(ctx context.Context, worktree string, plan Plan) (validateResult, error) {
	meta, err := pack.ReadMetaFrom(worktree)
	if err != nil {
		return validateResult{}, err
	}
	lock, err := pack.ReadLockFrom(worktree)
	if err != nil {
		return validateResult{}, err
	}

	// V1: schema re-validation (worker-side).
	if err := validatePlanSchema(plan); err != nil {
		return validateResult{}, fmt.Errorf("V1 plan schema: %w", err)
	}

	// V2: diff path allowlist + text + size.
	changed, err := pack.DiffNames(ctx, worktree)
	if err != nil {
		return validateResult{}, fmt.Errorf("V2 git diff: %w", err)
	}
	for _, p := range changed {
		if !underAllowed(p) {
			return validateResult{}, fmt.Errorf("V2: change to disallowed path %q", p)
		}
		full := filepath.Join(worktree, filepath.FromSlash(p))
		info, statErr := os.Stat(full)
		if statErr != nil {
			continue // deletions can't be stat'd; allowed dirs already checked
		}
		if info.Size() > 1<<20 {
			return validateResult{}, fmt.Errorf("V2: %q exceeds 1 MiB", p)
		}
		buf, _ := os.ReadFile(full)
		if !utf8.Valid(buf) {
			return validateResult{}, fmt.Errorf("V2: %q is not valid UTF-8", p)
		}
	}

	// V3: bijection changed paths == config_edit paths.
	editPaths := map[string]bool{}
	for _, a := range plan.Actions {
		if a.Type == ActConfigEdit {
			editPaths[a.Path] = true
		}
	}
	changedSet := map[string]bool{}
	for _, p := range changed {
		changedSet[p] = true
	}
	for p := range editPaths {
		if !changedSet[p] {
			return validateResult{}, fmt.Errorf("V3: config_edit for %q but the file was not changed", p)
		}
	}
	for p := range changedSet {
		if !editPaths[p] {
			return validateResult{}, fmt.Errorf("V3: %q changed but no config_edit action recorded it", p)
		}
	}

	// V4: parse each changed file per extension.
	for _, p := range changed {
		full := filepath.Join(worktree, filepath.FromSlash(p))
		if _, statErr := os.Stat(full); statErr != nil {
			continue // deleted
		}
		if err := parseByExt(full, p); err != nil {
			return validateResult{}, fmt.Errorf("V4 %q: %w", p, err)
		}
	}

	res := validateResult{Plan: plan}

	// V5 + V6: resolve/verify mod actions.
	inLock := map[string]bool{} // project ids present
	byPath := map[string]pack.LockEntry{}
	for _, m := range lock.Mods {
		if m.Source.ProjectID != "" {
			inLock[m.Source.Platform+":"+m.Source.ProjectID] = true
		}
		byPath[m.Path] = m
	}

	for _, a := range plan.Actions {
		switch a.Type {
		case ActAddMod:
			if inLock[a.Platform+":"+a.ProjectID] {
				return validateResult{}, fmt.Errorf("V5: add_mod for a project already installed (%s); use update_mod", a.ProjectSlug)
			}
			rm, err := d.resolveMod(ctx, meta, a.Platform, a.ProjectID, a.VersionID)
			if err != nil {
				return validateResult{}, fmt.Errorf("V5 add_mod %s: %w", a.ProjectSlug, err)
			}
			res.Adds = append(res.Adds, rm)
		case ActUpdateMod:
			if _, ok := byPath[a.FromPath]; !ok {
				return validateResult{}, fmt.Errorf("V6: update_mod from_path %q not installed", a.FromPath)
			}
			rm, err := d.resolveMod(ctx, meta, a.Platform, a.ProjectID, a.VersionID)
			if err != nil {
				return validateResult{}, fmt.Errorf("V5 update_mod: %w", err)
			}
			res.Adds = append(res.Adds, rm)
			res.Removes = append(res.Removes, a.FromPath)
		case ActRemoveMod:
			if _, ok := byPath[a.Path]; !ok {
				return validateResult{}, fmt.Errorf("V6: remove_mod path %q not installed", a.Path)
			}
			res.Removes = append(res.Removes, a.Path)
		case ActServerProperty:
			res.ServerProps = append(res.ServerProps, a)
		}
	}

	// V7: dependency closure (auto-add required deps).
	autoAdds, extraActions, err := d.resolveDependencies(ctx, meta, lock, res.Adds)
	if err != nil {
		return validateResult{}, fmt.Errorf("V7 dependency closure: %w", err)
	}
	res.Adds = append(res.Adds, autoAdds...)
	res.Plan.Actions = append(res.Plan.Actions, extraActions...)

	// V8: server_property checks.
	if len(res.ServerProps) > 0 {
		existing, err := readServerProps(filepath.Join(d.Cfg.Minecraft.ServerDir, "server.properties"))
		if err != nil {
			return validateResult{}, fmt.Errorf("V8: read server.properties: %w", err)
		}
		for _, a := range res.ServerProps {
			if serverPropBlocklist[a.Key] {
				return validateResult{}, fmt.Errorf("V8: server property %q is not allowed", a.Key)
			}
			if _, ok := existing[a.Key]; !ok {
				return validateResult{}, fmt.Errorf("V8: server property %q does not exist", a.Key)
			}
		}
	}

	return res, nil
}

// resolveMod re-fetches a version from the registry and classifies its side.
func (d Deps) resolveMod(ctx context.Context, meta pack.PackMeta, platform, projectID, versionID string) (resolvedMod, error) {
	v, err := d.Registry.GetVersion(ctx, platform, projectID, versionID)
	if err != nil {
		return resolvedMod{}, err
	}
	if v.ProjectID != "" && projectID != "" && v.ProjectID != projectID {
		return resolvedMod{}, fmt.Errorf("version %s does not belong to project %s", versionID, projectID)
	}
	if !contains(v.MCVersions, meta.MCVersion) {
		return resolvedMod{}, fmt.Errorf("version %s does not support Minecraft %s", versionID, meta.MCVersion)
	}
	if !containsFold(v.Loaders, meta.Loader.ID) {
		return resolvedMod{}, fmt.Errorf("version %s does not support loader %s", versionID, meta.Loader.ID)
	}
	side := d.classifyProjectSide(ctx, platform, projectID)
	return resolvedMod{Platform: platform, Version: v, Side: side}, nil
}

// classifyProjectSide fetches project side metadata; defaults to both on error.
func (d Deps) classifyProjectSide(ctx context.Context, platform, projectID string) string {
	p, err := d.Registry.Project(ctx, platform, projectID)
	if err != nil {
		return pack.SideBoth
	}
	if p.ServerSide == "unsupported" {
		return pack.SideClient
	}
	if p.ClientSide == "unsupported" {
		return pack.SideServer
	}
	return pack.SideBoth
}

func underAllowed(p string) bool {
	for _, pre := range allowedEditPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

func parseByExt(full, rel string) error {
	buf, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".json":
		var v any
		return json.Unmarshal(buf, &v)
	case ".toml":
		var v any
		return toml.Unmarshal(buf, &v)
	case ".yaml", ".yml":
		var v any
		return yaml.Unmarshal(buf, &v)
	case ".json5", ".snbt":
		return checkBalanced(buf)
	case ".properties", ".cfg":
		return nil // lenient
	default:
		return fmt.Errorf("unknown config extension %q", filepath.Ext(rel))
	}
}

// checkBalanced is a lenient structural check for JSON5/SNBT: balanced
// braces/brackets and even quote count outside comments.
func checkBalanced(buf []byte) error {
	depth := 0
	for _, r := range string(buf) {
		switch r {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth < 0 {
				return fmt.Errorf("unbalanced brackets")
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("unbalanced brackets")
	}
	return nil
}

func readServerProps(path string) (map[string]string, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "="); i >= 0 {
			out[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
	return out, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func containsFold(ss []string, s string) bool {
	for _, x := range ss {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
