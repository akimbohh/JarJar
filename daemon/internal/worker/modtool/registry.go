// Package modtool provides the Modrinth and CurseForge registry clients. It is
// the model's only window to the outside world (via the `jarjard modtool` CLI)
// and the same code the validator uses to re-verify plans, which is what makes
// "the model can only cite what the registry actually said" enforceable
// (PIPELINE.md §8).
package modtool

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Platform identifiers.
const (
	PlatformModrinth   = "modrinth"
	PlatformCurseForge = "curseforge"
	PlatformManual     = "manual"
)

// Dependency kinds.
const (
	DepRequired     = "required"
	DepOptional     = "optional"
	DepIncompatible = "incompatible"
)

// ErrNotFound is returned when a project/version does not exist.
var ErrNotFound = errors.New("registry: not found")

// SearchResult is one row of `modtool search`.
type SearchResult struct {
	Platform    string `json:"platform"`
	ProjectID   string `json:"project_id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Downloads   int    `json:"downloads"`
	ClientSide  string `json:"client_side"`
	ServerSide  string `json:"server_side"`
}

// Project is full project metadata (`modtool project`).
type Project struct {
	Platform    string   `json:"platform"`
	ProjectID   string   `json:"project_id"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Categories  []string `json:"categories"`
	ClientSide  string   `json:"client_side"`
	ServerSide  string   `json:"server_side"`
	SourceURL   string   `json:"source_url"`
	PageURL     string   `json:"page_url"`
}

// Version is one concrete installable version (`modtool versions`). Fields with
// json:"-" are used internally by materialize/validate but not exposed to the
// model.
type Version struct {
	VersionID     string       `json:"version_id"`
	VersionNumber string       `json:"version_number"`
	MCVersions    []string     `json:"mc_versions"`
	Loaders       []string     `json:"loaders"`
	Date          string       `json:"date"`
	FileName      string       `json:"file_name"`
	Dependencies  []Dependency `json:"dependencies"`

	ProjectID   string `json:"-"`
	DownloadURL string `json:"-"`
	SHA512      string `json:"-"`
	SHA1        string `json:"-"`
	Size        int64  `json:"-"`
}

type Dependency struct {
	ProjectID string `json:"project_id"`
	Kind      string `json:"kind"`
}

// Registry is the combined client. CurseForge is used only when allowCF is set.
type Registry struct {
	http    *http.Client
	allowCF bool
	cfKey   string
	ua      string
}

// New constructs a Registry. userAgent should identify JarJar per registry
// etiquette.
func New(allowCF bool, cfKey, userAgent string) *Registry {
	return &Registry{
		http:    &http.Client{Timeout: 15 * time.Second},
		allowCF: allowCF,
		cfKey:   cfKey,
		ua:      userAgent,
	}
}

// Search queries enabled platforms, pre-filtered to mc + loader, Modrinth first.
func (r *Registry) Search(ctx context.Context, query, mc, loader string, limit int) ([]SearchResult, error) {
	results, err := r.modrinthSearch(ctx, query, mc, loader, limit)
	if err != nil {
		return nil, err
	}
	if r.allowCF {
		cf, cfErr := r.curseforgeSearch(ctx, query, mc, loader, limit)
		if cfErr == nil {
			results = append(results, cf...)
		}
	}
	return results, nil
}

// Project fetches full metadata for one project.
func (r *Registry) Project(ctx context.Context, platform, projectID string) (Project, error) {
	switch platform {
	case PlatformModrinth:
		return r.modrinthProject(ctx, projectID)
	case PlatformCurseForge:
		if !r.allowCF {
			return Project{}, errors.New("curseforge is disabled")
		}
		return r.curseforgeProject(ctx, projectID)
	default:
		return Project{}, errors.New("unknown platform")
	}
}

// Versions lists compatible versions newest-first.
func (r *Registry) Versions(ctx context.Context, platform, projectID, mc, loader string, limit int) ([]Version, error) {
	switch platform {
	case PlatformModrinth:
		return r.modrinthVersions(ctx, projectID, mc, loader, limit)
	case PlatformCurseForge:
		if !r.allowCF {
			return nil, errors.New("curseforge is disabled")
		}
		return r.curseforgeVersions(ctx, projectID, mc, loader, limit)
	default:
		return nil, errors.New("unknown platform")
	}
}

// GetVersion fetches one specific version by id (validation re-verification).
// For CurseForge, projectID (the mod id) is required alongside versionID (the
// file id).
func (r *Registry) GetVersion(ctx context.Context, platform, projectID, versionID string) (Version, error) {
	switch platform {
	case PlatformModrinth:
		return r.modrinthVersion(ctx, versionID)
	case PlatformCurseForge:
		if !r.allowCF {
			return Version{}, errors.New("curseforge is disabled")
		}
		return r.curseforgeFile(ctx, projectID, versionID)
	default:
		return Version{}, errors.New("unknown platform")
	}
}
