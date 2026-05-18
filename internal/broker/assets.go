package broker

import (
	"archive/zip"
	"bytes"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// SkillFS holds the embedded skill SDK directory. It is populated by the
// top-level main package (which actually hosts the //go:embed directive
// because go:embed only works from the package that owns the files).
var SkillFS fs.FS

// SetSkillFS is called once at startup from main.
func SetSkillFS(f fs.FS) { SkillFS = f }

//go:embed landing.html
var landingFS embed.FS

func landingHTML() []byte {
	b, _ := landingFS.ReadFile("landing.html")
	return b
}

// serveLanding renders the human-readable intro page. It substitutes a
// {{BASE}} placeholder with the X-Forwarded-Prefix header so links work
// correctly behind a reverse proxy, and also injects live counters
// ({{STATS_ONLINE}}, etc.) so the landing shows current activity without
// requiring client-side JS.
func (b *Broker) serveLanding(c *gin.Context) {
	// Count this as a page view. Best-effort — ignore error.
	_ = b.Store.IncrStat("visits", 1)

	base := b.prefixPath(c)
	stats := b.collectStats()
	html := landingHTML()
	html = bytes.ReplaceAll(html, []byte("{{BASE}}"), []byte(base))
	html = bytes.ReplaceAll(html, []byte("{{STATS_ONLINE}}"), []byte(fmt.Sprintf("%d", stats.Agents.Online)))
	html = bytes.ReplaceAll(html, []byte("{{STATS_AGENTS_TOTAL}}"), []byte(fmt.Sprintf("%d", stats.Agents.Total)))
	html = bytes.ReplaceAll(html, []byte("{{STATS_TASKS}}"), []byte(fmt.Sprintf("%d", stats.Tasks.Total)))
	html = bytes.ReplaceAll(html, []byte("{{STATS_VISITS}}"), []byte(fmt.Sprintf("%d", stats.Visits)))
	c.Data(http.StatusOK, "text/html; charset=utf-8", html)
}

// ServeLandingHTTP is the exported handler form used by main when mounting
// the landing page on additional routes (e.g. GET /a2a).
func (b *Broker) ServeLandingHTTP(c *gin.Context) { b.serveLanding(c) }

// serveSDKFile streams an embedded skill file by relative path.
func (b *Broker) serveSDKFile(c *gin.Context) {
	if SkillFS == nil {
		c.String(http.StatusServiceUnavailable, "skill SDK not embedded")
		return
	}
	rel := strings.TrimPrefix(c.Param("path"), "/")
	if strings.Contains(rel, "..") {
		c.String(http.StatusBadRequest, "invalid path")
		return
	}
	if rel == "" {
		b.serveSDKIndex(c)
		return
	}
	f, err := SkillFS.Open(rel)
	if err != nil {
		c.String(http.StatusNotFound, "not found: %s", rel)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	if info != nil && info.IsDir() {
		b.serveSDKIndex(c)
		return
	}
	ct := "text/plain; charset=utf-8"
	switch {
	case strings.HasSuffix(rel, ".json"):
		ct = "application/json"
	case strings.HasSuffix(rel, ".md"):
		ct = "text/markdown; charset=utf-8"
	case strings.HasSuffix(rel, ".go"):
		ct = "text/x-go; charset=utf-8"
	case strings.HasSuffix(rel, ".sh"):
		ct = "text/x-shellscript; charset=utf-8"
	case strings.HasSuffix(rel, ".html"):
		ct = "text/html; charset=utf-8"
	}
	data, _ := io.ReadAll(f)
	c.Data(http.StatusOK, ct, data)
}

// serveSDKIndex lists the files in the embedded SDK directory as HTML.
func (b *Broker) serveSDKIndex(c *gin.Context) {
	if SkillFS == nil {
		c.String(http.StatusServiceUnavailable, "skill SDK not embedded")
		return
	}
	var rows strings.Builder
	_ = fs.WalkDir(SkillFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "." {
			return nil
		}
		info, _ := d.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		fmt.Fprintf(&rows, `<tr><td><a href="%s/sdk/%s">%s</a></td><td>%d bytes</td></tr>`,
			b.prefixPath(c), p, p, size)
		return nil
	})
	html := fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><title>A2A Broker SDK — files</title>
<style>body{font:14px/1.5 ui-monospace,Menlo,monospace;max-width:800px;margin:2em auto;padding:0 1em;color:#222}a{color:#06c}table{border-collapse:collapse;width:100%%}td{padding:.25em .5em;border-bottom:1px solid #eee}</style>
</head><body><h1>A2A Broker SDK</h1>
<p>Individual skill files. One-shot zip: <a href="%s/download/a2a-broker-sdk.zip">a2a-broker-sdk.zip</a>.</p>
<table>%s</table></body></html>`, b.prefixPath(c), rows.String())
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
}

// serveSDKZip streams the embedded SDK as a zip archive.
func (b *Broker) serveSDKZip(c *gin.Context) {
	if SkillFS == nil {
		c.String(http.StatusServiceUnavailable, "skill SDK not embedded")
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	err := fs.WalkDir(SkillFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || p == "." {
			return nil
		}
		f, err := SkillFS.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := &zip.FileHeader{
			Name:     path.Join("a2a-broker-sdk", p),
			Method:   zip.Deflate,
			Modified: time.Now(),
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		c.String(http.StatusInternalServerError, "zip error: %v", err)
		return
	}
	_ = zw.Close()

	c.Header("Content-Disposition", `attachment; filename="a2a-broker-sdk.zip"`)
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}

// prefixPath returns the URL prefix the client used to reach us, so we can
// build self-referential links that survive an upstream reverse-proxy. It
// honors the X-Forwarded-Prefix header (set by the edge proxy) if present;
// otherwise returns "".
func (b *Broker) prefixPath(c *gin.Context) string {
	p := c.GetHeader("X-Forwarded-Prefix")
	return strings.TrimRight(p, "/")
}
