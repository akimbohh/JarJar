package modtool

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

const modrinthBase = "https://api.modrinth.com/v2"

// loaderCategory maps our loader ids to Modrinth's category facet values.
func loaderCategory(loader string) string { return loader }

type mrSearchResponse struct {
	Hits []mrHit `json:"hits"`
}

type mrHit struct {
	ProjectID   string `json:"project_id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Downloads   int    `json:"downloads"`
	ClientSide  string `json:"client_side"`
	ServerSide  string `json:"server_side"`
}

// modrinthFacets builds a Modrinth facet array for the given project type,
// including the mc/loader facets only when they are set (genesis search runs
// before the loader/version are chosen, so they may be empty).
func modrinthFacets(projectType, mc, loader string) string {
	parts := []string{fmt.Sprintf(`["project_type:%s"]`, projectType)}
	if loader != "" {
		parts = append(parts, fmt.Sprintf(`["categories:%s"]`, loaderCategory(loader)))
	}
	if mc != "" {
		parts = append(parts, fmt.Sprintf(`["versions:%s"]`, mc))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func (r *Registry) modrinthSearch(ctx context.Context, query, mc, loader string, limit int) ([]SearchResult, error) {
	facets := modrinthFacets("mod", mc, loader)
	u := fmt.Sprintf("%s/search?query=%s&facets=%s&limit=%d",
		modrinthBase, url.QueryEscape(query), url.QueryEscape(facets), limit)
	var resp mrSearchResponse
	if err := r.getJSON(ctx, u, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		out = append(out, SearchResult{
			Platform:    PlatformModrinth,
			ProjectID:   h.ProjectID,
			Slug:        h.Slug,
			Title:       h.Title,
			Description: h.Description,
			Downloads:   h.Downloads,
			ClientSide:  h.ClientSide,
			ServerSide:  h.ServerSide,
		})
	}
	return out, nil
}

type mrProject struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Categories  []string `json:"categories"`
	ClientSide  string   `json:"client_side"`
	ServerSide  string   `json:"server_side"`
	SourceURL   string   `json:"source_url"`
}

func (r *Registry) modrinthProject(ctx context.Context, id string) (Project, error) {
	var p mrProject
	if err := r.getJSON(ctx, modrinthBase+"/project/"+url.PathEscape(id), nil, &p); err != nil {
		return Project{}, err
	}
	return Project{
		Platform:    PlatformModrinth,
		ProjectID:   p.ID,
		Slug:        p.Slug,
		Title:       p.Title,
		Description: p.Description,
		Categories:  p.Categories,
		ClientSide:  p.ClientSide,
		ServerSide:  p.ServerSide,
		SourceURL:   p.SourceURL,
		PageURL:     "https://modrinth.com/mod/" + p.Slug,
	}, nil
}

type mrVersion struct {
	ID            string   `json:"id"`
	ProjectID     string   `json:"project_id"`
	VersionNumber string   `json:"version_number"`
	GameVersions  []string `json:"game_versions"`
	Loaders       []string `json:"loaders"`
	DatePublished string   `json:"date_published"`
	Files         []mrFile `json:"files"`
	Dependencies  []mrDep  `json:"dependencies"`
}

type mrFile struct {
	Filename string            `json:"filename"`
	URL      string            `json:"url"`
	Primary  bool              `json:"primary"`
	Size     int64             `json:"size"`
	Hashes   map[string]string `json:"hashes"`
}

type mrDep struct {
	ProjectID      string `json:"project_id"`
	DependencyType string `json:"dependency_type"` // required|optional|incompatible|embedded
}

func (r *Registry) modrinthVersions(ctx context.Context, projectID, mc, loader string, limit int) ([]Version, error) {
	loaders := fmt.Sprintf(`["%s"]`, loader)
	gvs := fmt.Sprintf(`["%s"]`, mc)
	u := fmt.Sprintf("%s/project/%s/version?loaders=%s&game_versions=%s",
		modrinthBase, url.PathEscape(projectID), url.QueryEscape(loaders), url.QueryEscape(gvs))
	var vs []mrVersion
	if err := r.getJSON(ctx, u, nil, &vs); err != nil {
		return nil, err
	}
	out := make([]Version, 0, len(vs))
	for _, v := range vs {
		out = append(out, convertMrVersion(v))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (r *Registry) modrinthVersion(ctx context.Context, versionID string) (Version, error) {
	var v mrVersion
	if err := r.getJSON(ctx, modrinthBase+"/version/"+url.PathEscape(versionID), nil, &v); err != nil {
		return Version{}, err
	}
	return convertMrVersion(v), nil
}

func convertMrVersion(v mrVersion) Version {
	out := Version{
		VersionID:     v.ID,
		ProjectID:     v.ProjectID,
		VersionNumber: v.VersionNumber,
		MCVersions:    v.GameVersions,
		Loaders:       v.Loaders,
		Date:          v.DatePublished,
	}
	// Choose the primary file, else the first.
	var chosen *mrFile
	for i := range v.Files {
		if v.Files[i].Primary {
			chosen = &v.Files[i]
			break
		}
	}
	if chosen == nil && len(v.Files) > 0 {
		chosen = &v.Files[0]
	}
	if chosen != nil {
		out.FileName = chosen.Filename
		out.DownloadURL = chosen.URL
		out.Size = chosen.Size
		out.SHA512 = chosen.Hashes["sha512"]
		out.SHA1 = chosen.Hashes["sha1"]
	}
	for _, d := range v.Dependencies {
		kind := d.DependencyType
		if kind == "embedded" {
			continue // bundled inside the jar; not a separate download
		}
		if d.ProjectID == "" {
			continue
		}
		out.Dependencies = append(out.Dependencies, Dependency{ProjectID: d.ProjectID, Kind: normalizeDep(kind)})
	}
	return out
}

func normalizeDep(k string) string {
	switch strings.ToLower(k) {
	case "required":
		return DepRequired
	case "optional":
		return DepOptional
	case "incompatible":
		return DepIncompatible
	default:
		return DepOptional
	}
}
