package api

import (
	"embed"
	"io/fs"
	"strings"
)

// staticFiles embeds the dashboard's stylesheet, script, and fonts. The
// dashboard is the resurrected pre-rewrite single-page UI adapted for the FAB
// architecture: every asset is served from this binary under a strict CSP with
// no inline script or style, and every data request goes to the authenticated
// read-only /api views with the browser-held bearer token.
//
//go:embed static
var staticFiles embed.FS

type staticAsset struct {
	contentType string
	body        []byte
}

// staticAssets is materialized once at startup from the embedded read-only
// filesystem. Request handling only ever performs a lookup in this fixed map,
// so no request-derived value can influence a file read or the served bytes.
var staticAssets = loadStaticAssets()

func loadStaticAssets() map[string]staticAsset {
	assets := map[string]staticAsset{}
	_ = fs.WalkDir(staticFiles, "static", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		kind := ""
		switch {
		case strings.HasSuffix(name, ".css"):
			kind = "text/css; charset=utf-8"
		case strings.HasSuffix(name, ".js"):
			kind = "text/javascript; charset=utf-8"
		case strings.HasSuffix(name, ".woff2"):
			kind = "font/woff2"
		default:
			return nil
		}
		body, readErr := staticFiles.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		assets[strings.TrimPrefix(name, "static/")] = staticAsset{contentType: kind, body: body}
		return nil
	})
	return assets
}
