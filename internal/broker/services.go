package broker

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// Service is one logical skill offered by one or more agents.
type Service struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Tags        []string           `json:"tags,omitempty"`
	Providers   []ServiceProvider  `json:"providers"`
}

// ServiceProvider is one agent that offers a service.
type ServiceProvider struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	Online  bool   `json:"online"`
	Mode    string `json:"mode"`
	A2AURL  string `json:"a2a_url"`
}

// collectServices aggregates skills across every registered agent. Optional
// filters: tag (substring match against id/name/tags), onlineOnly (skip
// providers that aren't currently connected, and drop services with no
// remaining providers).
func (b *Broker) collectServices(tag string, onlineOnly bool) []Service {
	agents, err := b.Store.ListAgents(false)
	if err != nil {
		return nil
	}

	tag = strings.ToLower(strings.TrimSpace(tag))
	byID := map[string]*Service{}

	for _, a := range agents {
		if onlineOnly && !a.OnlineRT {
			continue
		}
		var card struct {
			Skills []struct {
				ID          string   `json:"id"`
				Name        string   `json:"name"`
				Description string   `json:"description"`
				Tags        []string `json:"tags"`
			} `json:"skills"`
		}
		if err := json.Unmarshal(a.CardJSON, &card); err != nil {
			continue
		}
		for _, sk := range card.Skills {
			if tag != "" && !skillMatches(sk.ID, sk.Name, sk.Tags, tag) {
				continue
			}
			key := sk.ID
			if key == "" {
				key = sk.Name
			}
			s, ok := byID[key]
			if !ok {
				s = &Service{
					ID: sk.ID, Name: sk.Name,
					Description: sk.Description,
					Tags:        sk.Tags,
				}
				byID[key] = s
			}
			s.Providers = append(s.Providers, ServiceProvider{
				AgentID: a.ID,
				Name:    a.Name,
				Online:  a.OnlineRT,
				Mode:    a.Mode,
				A2AURL:  fmt.Sprintf("%s/agents/%s/a2a", b.PublicURL, a.ID),
			})
		}
	}

	out := make([]Service, 0, len(byID))
	for _, s := range byID {
		// Sort providers: online first, then by name.
		sort.SliceStable(s.Providers, func(i, j int) bool {
			if s.Providers[i].Online != s.Providers[j].Online {
				return s.Providers[i].Online
			}
			return s.Providers[i].Name < s.Providers[j].Name
		})
		out = append(out, *s)
	}
	// Sort services: by name, services with online providers first.
	sort.SliceStable(out, func(i, j int) bool {
		ai := anyOnline(out[i].Providers)
		aj := anyOnline(out[j].Providers)
		if ai != aj {
			return ai
		}
		return out[i].Display() < out[j].Display()
	})
	return out
}

func (s Service) Display() string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

func anyOnline(ps []ServiceProvider) bool {
	for _, p := range ps {
		if p.Online {
			return true
		}
	}
	return false
}

func skillMatches(id, name string, tags []string, needle string) bool {
	id = strings.ToLower(id)
	name = strings.ToLower(name)
	if strings.Contains(id, needle) || strings.Contains(name, needle) {
		return true
	}
	for _, t := range tags {
		if strings.Contains(strings.ToLower(t), needle) {
			return true
		}
	}
	return false
}

// handleListServicesJSON is the API form for agents.
func (b *Broker) handleListServicesJSON(c *gin.Context) {
	tag := c.Query("tag")
	if tag == "" {
		tag = c.Query("skill")
	}
	onlineOnly := c.Query("available") == "now"
	c.JSON(http.StatusOK, b.collectServices(tag, onlineOnly))
}

// handleServicesPage is the HTML form for humans.
func (b *Broker) handleServicesPage(c *gin.Context) {
	tag := c.Query("tag")
	if tag == "" {
		tag = c.Query("skill")
	}
	onlineOnly := c.Query("available") == "now"
	services := b.collectServices(tag, onlineOnly)

	base := b.prefixPath(c)
	stats := b.collectStats()

	var rows strings.Builder
	for _, s := range services {
		var providerHTML strings.Builder
		for _, p := range s.Providers {
			dot := "off"
			label := "offline"
			if p.Online {
				dot = "on"
				label = "online"
			}
			fmt.Fprintf(&providerHTML, `<a class="prov" href="%s/agents/%s" title="%s · %s">
  <span class="dot %s"></span>%s
</a>`,
				base, html.EscapeString(p.AgentID),
				html.EscapeString(p.AgentID), label,
				dot, html.EscapeString(p.Name))
		}

		// tag chips
		var tagsHTML strings.Builder
		for _, t := range s.Tags {
			fmt.Fprintf(&tagsHTML, `<a class="chip" href="%s/services?tag=%s">%s</a> `,
				base, html.EscapeString(t), html.EscapeString(t))
		}

		online := 0
		for _, p := range s.Providers {
			if p.Online {
				online++
			}
		}
		statusBadge := fmt.Sprintf(`<span class="badge %s">%d/%d</span>`,
			func() string {
				if online > 0 {
					return "live"
				}
				return "dim"
			}(),
			online, len(s.Providers))

		fmt.Fprintf(&rows, `
<div class="svc">
  <div class="svc-head">
    <div>
      <div class="svc-name">%s</div>
      <div class="svc-id muted small"><code class="inline">%s</code></div>
    </div>
    %s
  </div>
  <div class="svc-desc">%s</div>
  <div class="tags">%s</div>
  <div class="providers">%s</div>
</div>`,
			html.EscapeString(s.Display()),
			html.EscapeString(s.ID),
			statusBadge,
			html.EscapeString(s.Description),
			tagsHTML.String(),
			providerHTML.String(),
		)
	}

	if len(services) == 0 {
		filterMsg := ""
		if tag != "" || onlineOnly {
			filterMsg = fmt.Sprintf(" (tag=%q, available=%v)", tag, onlineOnly)
		}
		fmt.Fprintf(&rows,
			`<div class="empty">还没有匹配的 service%s · No matching services yet.</div>`,
			html.EscapeString(filterMsg))
	}

	// Template placeholders, in order: 3× base (top nav), 4× int (stats),
	// 1× base (form action), tag value, "checked" attr.
	header := fmt.Sprintf(servicesPageHeader,
		base, base, base,
		stats.Agents.Online, stats.Agents.Total, len(services), stats.Visits,
		base, html.EscapeString(tag), boolChecked(onlineOnly))

	c.Data(http.StatusOK, "text/html; charset=utf-8",
		[]byte(header+rows.String()+servicesPageFooter))
}

func boolChecked(b bool) string {
	if b {
		return " checked"
	}
	return ""
}

const servicesPageHeader = `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>Service 目录 · A2A Broker</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  :root { --fg:#0f172a; --muted:#64748b; --bg:#fafafa; --card:#fff; --accent:#0f62fe; --line:#e2e8f0; --good:#16a34a; --dim:#94a3b8 }
  * { box-sizing:border-box }
  html,body { margin:0; background:var(--bg); color:var(--fg);
    font:15px/1.55 "PingFang SC","Microsoft YaHei",-apple-system,"SF Pro Text",Segoe UI,Roboto,sans-serif }
  main { max-width: 1100px; margin: 2rem auto; padding: 0 1.2rem }
  h1 { font-size: 1.6rem; margin: 0 0 .25rem; display:flex; align-items:center; gap:.6rem }
  .pill { display:inline-block; background:#eef2ff; color:#3730a3; font-size:12px; padding:.15rem .5rem; border-radius:999px }
  .row { display:flex; gap:.5rem; flex-wrap:wrap; margin:.9rem 0; align-items:center }
  a.btn { background: var(--accent); color:white; padding:.45rem .8rem; border-radius:7px; text-decoration:none; font-weight:600; font-size:.9rem }
  a.btn.secondary { background:transparent; color: var(--accent); border: 1px solid var(--accent) }
  .stats { display:grid; grid-template-columns: repeat(4,minmax(0,1fr)); gap:.6rem; margin: 1.1rem 0 1.5rem }
  @media (max-width:640px) { .stats { grid-template-columns: repeat(2,1fr) } }
  .stat { background: var(--card); border:1px solid var(--line); border-radius:10px; padding:.7rem .9rem }
  .stat .n { font-size:1.55rem; font-weight:700; line-height:1.1; color: var(--accent) }
  .stat .k { color: var(--muted); font-size: .82rem; margin-top:.15rem }
  form.filter { display:flex; gap:.5rem; flex-wrap:wrap; align-items:center; background: var(--card); border:1px solid var(--line); border-radius:10px; padding:.6rem .8rem; margin:.8rem 0 1.5rem }
  form.filter input[type=text] { flex: 1 1 200px; min-width: 0; border:1px solid var(--line); padding:.4rem .6rem; border-radius:6px; font: inherit }
  form.filter label { color: var(--muted); font-size:.9rem }
  form.filter button { background: var(--accent); color:white; border:0; border-radius:6px; padding:.45rem 1rem; font-weight:600; cursor:pointer }
  .svc { background: var(--card); border:1px solid var(--line); border-radius:12px; padding:1rem 1.1rem; margin:.7rem 0 }
  .svc-head { display:flex; justify-content:space-between; align-items:flex-start; gap:1rem }
  .svc-name { font-size:1.05rem; font-weight:700 }
  .svc-id { margin-top:.1rem }
  .svc-desc { color:#334155; margin:.5rem 0 .7rem; font-size:.95rem }
  .badge { font-size:.75rem; padding:.2rem .55rem; border-radius:999px; font-weight:600; white-space: nowrap }
  .badge.live { background:#dcfce7; color:#15803d }
  .badge.dim  { background:#f1f5f9; color:#64748b }
  .tags { margin:.3rem 0 }
  .chip { display:inline-block; background:#eef2f7; color:#0f172a; font-size:.75rem; padding:.1rem .45rem; border-radius:999px; margin: 1px 2px 1px 0; text-decoration:none }
  .chip:hover { background:#dbeafe }
  .providers { display:flex; flex-wrap:wrap; gap:.4rem; margin-top:.6rem }
  .prov { background:#f8fafc; border:1px solid var(--line); padding:.25rem .55rem; border-radius:6px; font-size:.85rem; color:#0f172a; text-decoration:none }
  .prov:hover { border-color: var(--accent) }
  .dot { display:inline-block; width:.5rem; height:.5rem; border-radius:50%%; margin-right:.35rem; vertical-align: middle }
  .dot.on { background: var(--good); box-shadow:0 0 0 3px rgba(22,163,74,.15) }
  .dot.off { background: var(--dim) }
  .muted { color: var(--muted) } .small { font-size:.8rem }
  code, code.inline { font-family: ui-monospace,SFMono-Regular,Menlo,monospace; background:#eef2f7; padding:.1rem .3rem; border-radius:4px; font-size:.78rem }
  .empty { padding: 2rem; text-align:center; color: var(--muted); background: var(--card); border:1px dashed var(--line); border-radius:10px }
  footer { color: var(--muted); font-size: .82rem; margin: 2rem 0; text-align:center }
</style>
</head>
<body>
<main>

<h1>Service 目录 <span class="pill">A2A Broker</span></h1>
<p class="muted">所有注册 Agent 提供的能力按 skill 聚合 ·
<a href="%s/agents">Agent 视图</a> ·
<a href="%s/registry/services">JSON API</a> ·
<a href="%s/">返回首页</a></p>

<div class="stats">
  <div class="stat"><div class="n">%d</div><div class="k">在线 Agent · Online</div></div>
  <div class="stat"><div class="n">%d</div><div class="k">累计 Agent · Total</div></div>
  <div class="stat"><div class="n">%d</div><div class="k">Service 数 · Services</div></div>
  <div class="stat"><div class="n">%d</div><div class="k">页面访问 · Visits</div></div>
</div>

<form class="filter" action="%s/services" method="get">
  <label for="tag">筛选 / Filter:</label>
  <input id="tag" type="text" name="tag" placeholder="按 skill id/name/tag 过滤" value="%s">
  <label><input type="checkbox" name="available" value="now"%s> 仅在线 · online only</label>
  <button type="submit">查询</button>
</form>
`

const servicesPageFooter = `
<footer>支持 ?tag=&amp;available=now 过滤 · Supports ?tag= and ?available=now query params</footer>
</main>
</body>
</html>`
