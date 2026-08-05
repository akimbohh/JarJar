package main

import (
	"context"
	"io"

	"github.com/akimbohh/jarjar/daemon/internal/api"
	"github.com/akimbohh/jarjar/daemon/internal/pack"
)

// packAPI adapts *pack.Pack to the api.Pack interface, converting between the
// pack and api wire types so the api package stays decoupled from pack.
type packAPI struct {
	pk *pack.Pack
}

func (a packAPI) Meta(ctx context.Context) (api.PackMeta, bool, error) {
	name, mc, lid, lver, ok, err := a.pk.Meta(ctx)
	if err != nil || !ok {
		return api.PackMeta{}, ok, err
	}
	return api.PackMeta{Name: name, MCVersion: mc, Loader: api.Loader{ID: lid, Version: lver}}, true, nil
}

func (a packAPI) ManifestBytes(ctx context.Context, n int) ([]byte, error) {
	return a.pk.ManifestBytes(ctx, n)
}

func (a packAPI) Changelog(ctx context.Context, n int) ([]api.ChangelogEntry, error) {
	entries, err := a.pk.Changelog(ctx, n)
	if err != nil {
		return nil, err
	}
	out := make([]api.ChangelogEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, api.ChangelogEntry{Kind: e.Kind, Text: e.Text})
	}
	return out, nil
}

func (a packAPI) OpenBlob(sha string) (io.ReadSeekCloser, int64, error) {
	return a.pk.OpenBlob(sha)
}

func (a packAPI) BlobRetained(sha string) bool { return a.pk.BlobRetained(sha) }
