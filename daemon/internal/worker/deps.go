package worker

import (
	"context"
	"fmt"

	"github.com/akimbohh/jarjar/daemon/internal/pack"
	"github.com/akimbohh/jarjar/daemon/internal/worker/modtool"
)

const maxDepDepth = 10

// resolveDependencies computes the required-dependency closure of the mods being
// added, auto-adding any required project not already present or in the plan
// (V7). Returns the auto-added mods and synthetic add_mod actions recording them.
func (d Deps) resolveDependencies(ctx context.Context, meta pack.PackMeta, lock pack.ModsLock, adds []resolvedMod) ([]resolvedMod, []Action, error) {
	present := map[string]bool{} // platform:projectID
	for _, m := range lock.Mods {
		if m.Source.ProjectID != "" {
			present[m.Source.Platform+":"+m.Source.ProjectID] = true
		}
	}
	for _, a := range adds {
		if a.Version.ProjectID != "" {
			present[a.Platform+":"+a.Version.ProjectID] = true
		}
	}

	type queued struct {
		version  modtool.Version
		platform string
		depth    int
	}
	var queue []queued
	for _, a := range adds {
		queue = append(queue, queued{version: a.Version, platform: a.Platform, depth: 0})
	}

	var autoAdds []resolvedMod
	var actions []Action

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxDepDepth {
			continue
		}
		for _, dep := range cur.version.Dependencies {
			if dep.Kind != modtool.DepRequired || dep.ProjectID == "" {
				continue
			}
			key := cur.platform + ":" + dep.ProjectID
			if present[key] {
				continue
			}
			present[key] = true

			versions, err := d.Registry.Versions(ctx, cur.platform, dep.ProjectID, meta.MCVersion, meta.Loader.ID, 1)
			if err != nil || len(versions) == 0 {
				return nil, nil, fmt.Errorf("required dependency %s has no version compatible with Minecraft %s / %s", dep.ProjectID, meta.MCVersion, meta.Loader.ID)
			}
			depVer := versions[0]
			side := d.classifyProjectSide(ctx, cur.platform, dep.ProjectID)
			slug := dep.ProjectID
			if pr, perr := d.Registry.Project(ctx, cur.platform, dep.ProjectID); perr == nil {
				slug = pr.Slug
			}
			autoAdds = append(autoAdds, resolvedMod{
				Platform: cur.platform, ProjectSlug: slug, Side: side, Version: depVer, AutoAdded: true,
			})
			actions = append(actions, Action{
				Type: ActAddMod, Platform: cur.platform, ProjectID: dep.ProjectID,
				ProjectSlug: slug, VersionID: depVer.VersionID,
				Reason: "auto-added required dependency",
			})
			queue = append(queue, queued{version: depVer, platform: cur.platform, depth: cur.depth + 1})
		}
	}
	return autoAdds, actions, nil
}
