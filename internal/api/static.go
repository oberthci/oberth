package api

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"strings"
)

// staticFiles embeds the dashboard's stylesheet, script, and fonts. The
// dashboard is the single-page UI: every asset is served from this binary
// under a strict CSP with
// no inline script or style, and every data request goes to the authenticated
// read-only /api views with the browser-held bearer token.
//
//go:embed static
var staticFiles embed.FS

type staticAsset struct {
	contentType string
	body        []byte
	// version is a short content hash. The page links app.css and app.js
	// with ?v=<version>, so a release changes the URL and no cache (browser
	// or CDN) can serve the previous release's script (#054).
	version string
}

// versionedAssetURL is the cache-busting URL the page links an asset by.
func versionedAssetURL(name string) string {
	return "/assets/" + name + "?v=" + staticAssets[name].version
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
		sum := sha256.Sum256(body)
		assets[strings.TrimPrefix(name, "static/")] = staticAsset{contentType: kind, body: body, version: hex.EncodeToString(sum[:8])}
		return nil
	})
	return assets
}
