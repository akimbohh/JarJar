// Package pack owns the pack git repository, the content-addressed blob store,
// and version manifests. It implements the read surface the api package needs
// (api.Pack) and the build/import operations the worker and CLI need.
package pack

// PackMeta is jarjar.pack.json (committed at the pack repo root).
type PackMeta struct {
	SchemaVersion    int               `json:"schema_version"`
	Name             string            `json:"name"`
	MCVersion        string            `json:"mc_version"`
	Loader           Loader            `json:"loader"`
	SideOverrides    map[string]string `json:"side_overrides"`
	CurseForgeImport any               `json:"curseforge_import"`
}

type Loader struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// ModsLock is mods.lock.json (committed at the pack repo root).
type ModsLock struct {
	SchemaVersion int         `json:"schema_version"`
	Mods          []LockEntry `json:"mods"`
}

type LockEntry struct {
	Path   string     `json:"path"`
	SHA256 string     `json:"sha256"`
	Size   int64      `json:"size"`
	Side   string     `json:"side"`
	Source LockSource `json:"source"`
}

type LockSource struct {
	Platform    string `json:"platform"` // modrinth | curseforge | manual
	ProjectID   string `json:"project_id,omitempty"`
	ProjectSlug string `json:"project_slug,omitempty"`
	VersionID   string `json:"version_id,omitempty"` // modrinth
	FileID      string `json:"file_id,omitempty"`    // curseforge
	DownloadURL string `json:"download_url,omitempty"`
}

// Manifest mirrors schemas/manifest.schema.json.
type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	Pack          ManifestPack    `json:"pack"`
	Version       ManifestVersion `json:"version"`
	Files         []ManifestFile  `json:"files"`
}

type ManifestPack struct {
	Name      string `json:"name"`
	MCVersion string `json:"mc_version"`
	Loader    Loader `json:"loader"`
}

type ManifestVersion struct {
	Number    int              `json:"number"`
	CreatedAt string           `json:"created_at"`
	RequestID *string          `json:"request_id"`
	GitCommit string           `json:"git_commit"`
	Summary   string           `json:"summary"`
	Changelog []ChangelogEntry `json:"changelog"`
}

type ChangelogEntry struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type ManifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Side   string `json:"side"`
	Kind   string `json:"kind"`
}

// Side values.
const (
	SideClient = "client"
	SideServer = "server"
	SideBoth   = "both"
)
