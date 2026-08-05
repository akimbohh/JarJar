package modtool

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// PackResult is one row of a modpack search (`modtool packsearch`).
type PackResult struct {
	Platform    string `json:"platform"`
	ProjectID   string `json:"project_id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Downloads   int    `json:"downloads"`
}

// PackSearch searches published modpacks across enabled platforms, filtered to
// mc/loader when those are set (Modrinth first). Used by the genesis planner to
// find an existing pack to start from.
func (r *Registry) PackSearch(ctx context.Context, query, mc, loader string, limit int) ([]PackResult, error) {
	out, err := r.modrinthPackSearch(ctx, query, mc, loader, limit)
	if err != nil {
		return nil, err
	}
	if r.allowCF {
		if cf, cfErr := r.curseforgePackSearch(ctx, query, mc, loader, limit); cfErr == nil {
			out = append(out, cf...)
		}
	}
	return out, nil
}

func (r *Registry) modrinthPackSearch(ctx context.Context, query, mc, loader string, limit int) ([]PackResult, error) {
	facets := modrinthFacets("modpack", mc, loader)
	u := fmt.Sprintf("%s/search?query=%s&facets=%s&limit=%d",
		modrinthBase, url.QueryEscape(query), url.QueryEscape(facets), limit)
	var resp mrSearchResponse
	if err := r.getJSON(ctx, u, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]PackResult, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		out = append(out, PackResult{
			Platform: PlatformModrinth, ProjectID: h.ProjectID, Slug: h.Slug,
			Title: h.Title, Description: h.Description, Downloads: h.Downloads,
		})
	}
	return out, nil
}

// CurseForge Minecraft modpacks are classId 4471.
const cfModpackClassID = 4471

func (r *Registry) curseforgePackSearch(ctx context.Context, query, mc, loader string, limit int) ([]PackResult, error) {
	u := fmt.Sprintf("%s/v1/mods/search?gameId=%d&classId=%d&searchFilter=%s&pageSize=%d",
		curseforgeBase, cfMinecraftGameID, cfModpackClassID, url.QueryEscape(query), limit)
	if mc != "" {
		u += "&gameVersion=" + url.QueryEscape(mc)
	}
	if lt := cfLoaderType(loader); lt > 0 {
		u += "&modLoaderType=" + strconv.Itoa(lt)
	}
	var resp cfSearchResponse
	if err := r.getJSON(ctx, u, r.cfHeaders(), &resp); err != nil {
		return nil, err
	}
	out := make([]PackResult, 0, len(resp.Data))
	for _, m := range resp.Data {
		out = append(out, PackResult{
			Platform: PlatformCurseForge, ProjectID: strconv.Itoa(m.ID), Slug: m.Slug,
			Title: m.Name, Description: m.Summary, Downloads: m.DownloadCount,
		})
	}
	return out, nil
}

// PackVersions returns a pack's versions; the primary file of each is the
// .mrpack (Modrinth) or the pack archive (CurseForge). It reuses the mod
// Versions path — for a modpack project the primary file is the pack archive
// and DownloadURL points at it.
func (r *Registry) PackVersions(ctx context.Context, platform, projectID, mc, loader string, limit int) ([]Version, error) {
	return r.Versions(ctx, platform, projectID, mc, loader, limit)
}
