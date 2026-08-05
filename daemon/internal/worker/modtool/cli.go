package modtool

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// packCtx is the mc_version/loader read from jarjar.pack.json in the CWD (used
// as defaults for --mc-version/--loader).
type packCtx struct {
	MCVersion string `json:"mc_version"`
	Loader    struct {
		ID string `json:"id"`
	} `json:"loader"`
}

func readPackCtx() packCtx {
	var pc packCtx
	if buf, err := os.ReadFile(filepath.Join(".", "jarjar.pack.json")); err == nil {
		json.Unmarshal(buf, &pc)
	}
	return pc
}

// RunCLI implements `jarjard modtool <subcommand> ...`. It is invoked both by
// operators and, in the sandbox, by the model. CF settings come from the
// environment (JARJAR_ALLOW_CF, JARJAR_CF_KEY) that the worker sets; pack
// context defaults from jarjar.pack.json in the CWD. It prints a single JSON
// document to out and returns a process exit code.
func RunCLI(args []string, out, errOut io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(errOut, `usage: jarjard modtool <search|project|versions|installed> ...`)
		return 1
	}
	sub, rest := args[0], args[1:]

	pc := readPackCtx()
	allowCF := os.Getenv("JARJAR_ALLOW_CF") == "1"
	cfKey := os.Getenv("JARJAR_CF_KEY")
	reg := New(allowCF, cfKey, "jarjar/dev (github.com/akimbohh/JarJar)")
	ctx := context.Background()

	fail := func(msg string) int {
		enc := json.NewEncoder(out)
		enc.Encode(map[string]string{"error": msg})
		return 1
	}
	emit := func(v any) int {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return fail(err.Error())
		}
		return 0
	}

	switch sub {
	case "search":
		fs := flag.NewFlagSet("search", flag.ContinueOnError)
		fs.SetOutput(errOut)
		mc := fs.String("mc-version", pc.MCVersion, "")
		loader := fs.String("loader", pc.Loader.ID, "")
		limit := fs.Int("limit", 8, "")
		if err := fs.Parse(rest); err != nil {
			return 1
		}
		if fs.NArg() < 1 {
			return fail("usage: modtool search <query>")
		}
		results, err := reg.Search(ctx, fs.Arg(0), *mc, *loader, *limit)
		if err != nil {
			return fail(err.Error())
		}
		return emit(map[string]any{"results": results})

	case "project":
		if len(rest) < 2 {
			return fail("usage: modtool project <platform> <project_id>")
		}
		p, err := reg.Project(ctx, rest[0], rest[1])
		if err != nil {
			return fail(err.Error())
		}
		return emit(p)

	case "versions":
		fs := flag.NewFlagSet("versions", flag.ContinueOnError)
		fs.SetOutput(errOut)
		mc := fs.String("mc-version", pc.MCVersion, "")
		loader := fs.String("loader", pc.Loader.ID, "")
		limit := fs.Int("limit", 5, "")
		platform, projectID := "", ""
		// positional args come before flags in our usage; support both orders.
		var pos []string
		for _, a := range rest {
			if len(a) > 0 && a[0] == '-' {
				fs.Parse([]string{a})
				continue
			}
			pos = append(pos, a)
		}
		fs.Parse(rest)
		if len(pos) < 2 {
			return fail("usage: modtool versions <platform> <project_id>")
		}
		platform, projectID = pos[0], pos[1]
		versions, err := reg.Versions(ctx, platform, projectID, *mc, *loader, *limit)
		if err != nil {
			return fail(err.Error())
		}
		return emit(map[string]any{"versions": versions})

	case "packsearch":
		fs := flag.NewFlagSet("packsearch", flag.ContinueOnError)
		fs.SetOutput(errOut)
		mc := fs.String("mc-version", pc.MCVersion, "")
		loader := fs.String("loader", pc.Loader.ID, "")
		limit := fs.Int("limit", 8, "")
		if err := fs.Parse(rest); err != nil {
			return 1
		}
		if fs.NArg() < 1 {
			return fail("usage: modtool packsearch <query>")
		}
		results, err := reg.PackSearch(ctx, fs.Arg(0), *mc, *loader, *limit)
		if err != nil {
			return fail(err.Error())
		}
		return emit(map[string]any{"results": results})

	case "packversions":
		fs := flag.NewFlagSet("packversions", flag.ContinueOnError)
		fs.SetOutput(errOut)
		mc := fs.String("mc-version", pc.MCVersion, "")
		loader := fs.String("loader", pc.Loader.ID, "")
		limit := fs.Int("limit", 5, "")
		var pos []string
		for _, a := range rest {
			if len(a) > 0 && a[0] == '-' {
				continue
			}
			pos = append(pos, a)
		}
		fs.Parse(rest)
		if len(pos) < 2 {
			return fail("usage: modtool packversions <platform> <project_id>")
		}
		versions, err := reg.PackVersions(ctx, pos[0], pos[1], *mc, *loader, *limit)
		if err != nil {
			return fail(err.Error())
		}
		return emit(map[string]any{"versions": versions})

	case "installed":
		return emitInstalled(ctx, reg, emit, fail)

	default:
		return fail("unknown subcommand: " + sub)
	}
}

// emitInstalled renders mods.lock.json (from CWD) with project titles.
func emitInstalled(ctx context.Context, reg *Registry, emit func(any) int, fail func(string) int) int {
	buf, err := os.ReadFile("mods.lock.json")
	if err != nil {
		return emit(map[string]any{"mods": []any{}})
	}
	var lock struct {
		Mods []struct {
			Path   string `json:"path"`
			Side   string `json:"side"`
			Source struct {
				Platform  string `json:"platform"`
				ProjectID string `json:"project_id"`
				VersionID string `json:"version_id"`
				FileID    string `json:"file_id"`
			} `json:"source"`
		} `json:"mods"`
	}
	if err := json.Unmarshal(buf, &lock); err != nil {
		return fail("parse mods.lock.json: " + err.Error())
	}
	type row struct {
		Path      string `json:"path"`
		Platform  string `json:"platform"`
		ProjectID string `json:"project_id"`
		Title     string `json:"title"`
		VersionID string `json:"version_id,omitempty"`
		FileID    string `json:"file_id,omitempty"`
		Side      string `json:"side"`
	}
	out := make([]row, 0, len(lock.Mods))
	for _, m := range lock.Mods {
		title := ""
		if m.Source.Platform == PlatformModrinth || (m.Source.Platform == PlatformCurseForge && reg.allowCF) {
			if p, err := reg.Project(ctx, m.Source.Platform, m.Source.ProjectID); err == nil {
				title = p.Title
			}
		}
		out = append(out, row{
			Path:      m.Path,
			Platform:  m.Source.Platform,
			ProjectID: m.Source.ProjectID,
			Title:     title,
			VersionID: m.Source.VersionID,
			FileID:    m.Source.FileID,
			Side:      m.Side,
		})
	}
	return emit(map[string]any{"mods": out})
}
