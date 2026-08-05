package worker

import (
	"fmt"

	"github.com/akimbohh/jarjar/daemon/internal/pack"
)

// buildChangelog composes the version changelog from the plan actions
// (PIPELINE.md §5). The player-facing summary lives in the manifest separately.
func (d Deps) buildChangelog(plan Plan) []pack.ChangelogEntry {
	var out []pack.ChangelogEntry
	for _, a := range plan.Actions {
		switch a.Type {
		case ActAddMod:
			name := a.ProjectSlug
			if name == "" {
				name = a.ProjectID
			}
			text := "Added " + name
			if a.Reason == "auto-added required dependency" {
				text += " (required dependency)"
			}
			out = append(out, pack.ChangelogEntry{Kind: "added_mod", Text: text})
		case ActRemoveMod:
			out = append(out, pack.ChangelogEntry{Kind: "removed_mod", Text: "Removed " + baseName(a.Path)})
		case ActUpdateMod:
			out = append(out, pack.ChangelogEntry{Kind: "updated_mod", Text: "Updated " + baseName(a.FromPath)})
		case ActConfigEdit:
			text := a.Description
			if text == "" {
				text = "Edited " + a.Path
			}
			out = append(out, pack.ChangelogEntry{Kind: "config_changed", Text: text})
		case ActServerProperty:
			out = append(out, pack.ChangelogEntry{Kind: "server_setting", Text: fmt.Sprintf("Set %s = %s", a.Key, a.Value)})
		}
	}
	if len(out) == 0 {
		out = []pack.ChangelogEntry{{Kind: "other", Text: "No file changes"}}
	}
	return out
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
