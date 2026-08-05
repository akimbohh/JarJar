package modtool

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const curseforgeBase = "https://api.curseforge.com"
const cfMinecraftGameID = 432

// cfLoaderType maps loader ids to CurseForge modLoaderType ints.
func cfLoaderType(loader string) int {
	switch loader {
	case "forge":
		return 1
	case "fabric":
		return 4
	case "quilt":
		return 5
	case "neoforge":
		return 6
	default:
		return 0
	}
}

func (r *Registry) cfHeaders() map[string]string {
	return map[string]string{"x-api-key": r.cfKey}
}

type cfSearchResponse struct {
	Data []cfMod `json:"data"`
}

type cfMod struct {
	ID      int    `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Links   struct {
		WebsiteURL string `json:"websiteUrl"`
		SourceURL  string `json:"sourceUrl"`
	} `json:"links"`
	DownloadCount int          `json:"downloadCount"`
	Categories    []cfCategory `json:"categories"`
}

type cfCategory struct {
	Name string `json:"name"`
}

func (r *Registry) curseforgeSearch(ctx context.Context, query, mc, loader string, limit int) ([]SearchResult, error) {
	u := fmt.Sprintf("%s/v1/mods/search?gameId=%d&searchFilter=%s&gameVersion=%s&pageSize=%d",
		curseforgeBase, cfMinecraftGameID, url.QueryEscape(query), url.QueryEscape(mc), limit)
	if lt := cfLoaderType(loader); lt > 0 {
		u += "&modLoaderType=" + strconv.Itoa(lt)
	}
	var resp cfSearchResponse
	if err := r.getJSON(ctx, u, r.cfHeaders(), &resp); err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(resp.Data))
	for _, m := range resp.Data {
		out = append(out, SearchResult{
			Platform:    PlatformCurseForge,
			ProjectID:   strconv.Itoa(m.ID),
			Slug:        m.Slug,
			Title:       m.Name,
			Description: m.Summary,
			Downloads:   m.DownloadCount,
			// CurseForge exposes no side metadata; default both.
			ClientSide: "required",
			ServerSide: "required",
		})
	}
	return out, nil
}

type cfModResponse struct {
	Data cfMod `json:"data"`
}

func (r *Registry) curseforgeProject(ctx context.Context, projectID string) (Project, error) {
	var resp cfModResponse
	if err := r.getJSON(ctx, curseforgeBase+"/v1/mods/"+url.PathEscape(projectID), r.cfHeaders(), &resp); err != nil {
		return Project{}, err
	}
	m := resp.Data
	cats := make([]string, 0, len(m.Categories))
	for _, c := range m.Categories {
		cats = append(cats, c.Name)
	}
	return Project{
		Platform:    PlatformCurseForge,
		ProjectID:   strconv.Itoa(m.ID),
		Slug:        m.Slug,
		Title:       m.Name,
		Description: m.Summary,
		Categories:  cats,
		ClientSide:  "required",
		ServerSide:  "required",
		SourceURL:   m.Links.SourceURL,
		PageURL:     m.Links.WebsiteURL,
	}, nil
}

type cfFilesResponse struct {
	Data []cfFile `json:"data"`
}

type cfFileResponse struct {
	Data cfFile `json:"data"`
}

type cfFile struct {
	ID           int      `json:"id"`
	ModID        int      `json:"modId"`
	DisplayName  string   `json:"displayName"`
	FileName     string   `json:"fileName"`
	FileDate     string   `json:"fileDate"`
	FileLength   int64    `json:"fileLength"`
	DownloadURL  string   `json:"downloadUrl"`
	GameVersions []string `json:"gameVersions"` // contains both MC versions and loader names
	Hashes       []cfHash `json:"hashes"`
	Dependencies []cfDep  `json:"dependencies"`
}

type cfHash struct {
	Value string `json:"value"`
	Algo  int    `json:"algo"` // 1=sha1, 2=md5
}

type cfDep struct {
	ModID        int `json:"modId"`
	RelationType int `json:"relationType"` // 2=optional, 3=required, 5=incompatible
}

func (r *Registry) curseforgeVersions(ctx context.Context, projectID, mc, loader string, limit int) ([]Version, error) {
	u := fmt.Sprintf("%s/v1/mods/%s/files?gameVersion=%s", curseforgeBase, url.PathEscape(projectID), url.QueryEscape(mc))
	if lt := cfLoaderType(loader); lt > 0 {
		u += "&modLoaderType=" + strconv.Itoa(lt)
	}
	var resp cfFilesResponse
	if err := r.getJSON(ctx, u, r.cfHeaders(), &resp); err != nil {
		return nil, err
	}
	out := make([]Version, 0, len(resp.Data))
	for _, f := range resp.Data {
		if !cfCompatible(f, mc, loader) {
			continue
		}
		out = append(out, convertCfFile(f))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (r *Registry) curseforgeFile(ctx context.Context, projectID, fileID string) (Version, error) {
	var resp cfFileResponse
	u := fmt.Sprintf("%s/v1/mods/%s/files/%s", curseforgeBase, url.PathEscape(projectID), url.PathEscape(fileID))
	if err := r.getJSON(ctx, u, r.cfHeaders(), &resp); err != nil {
		return Version{}, err
	}
	return convertCfFile(resp.Data), nil
}

// cfCompatible checks the gameVersions list contains both the MC version and
// the loader name (CurseForge mixes them into one array).
func cfCompatible(f cfFile, mc, loader string) bool {
	hasMC, hasLoader := false, false
	for _, gv := range f.GameVersions {
		if gv == mc {
			hasMC = true
		}
		if strings.EqualFold(gv, loader) {
			hasLoader = true
		}
	}
	return hasMC && hasLoader
}

func convertCfFile(f cfFile) Version {
	v := Version{
		VersionID:     strconv.Itoa(f.ID),
		ProjectID:     strconv.Itoa(f.ModID),
		VersionNumber: f.DisplayName,
		Date:          f.FileDate,
		FileName:      f.FileName,
		DownloadURL:   f.DownloadURL,
		Size:          f.FileLength,
	}
	// Split gameVersions into MC versions and loaders (best effort).
	for _, gv := range f.GameVersions {
		if isLoaderName(gv) {
			v.Loaders = append(v.Loaders, strings.ToLower(gv))
		} else {
			v.MCVersions = append(v.MCVersions, gv)
		}
	}
	for _, h := range f.Hashes {
		if h.Algo == 1 {
			v.SHA1 = h.Value
		}
	}
	for _, d := range f.Dependencies {
		kind := ""
		switch d.RelationType {
		case 3:
			kind = DepRequired
		case 2:
			kind = DepOptional
		case 5:
			kind = DepIncompatible
		default:
			continue
		}
		v.Dependencies = append(v.Dependencies, Dependency{ProjectID: strconv.Itoa(d.ModID), Kind: kind})
	}
	return v
}

func isLoaderName(s string) bool {
	switch strings.ToLower(s) {
	case "forge", "fabric", "quilt", "neoforge":
		return true
	}
	return false
}
