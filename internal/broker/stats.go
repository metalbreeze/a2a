package broker

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// startedAt is set the first time any Broker instance starts serving, so we can
// compute an uptime for the /stats endpoint.
var startedAt = time.Now()

// handleStatsJSON returns raw counters as JSON — useful for dashboards or
// agent-side pollers.
func (b *Broker) handleStatsJSON(c *gin.Context) {
	stats := b.collectStats()
	c.JSON(http.StatusOK, stats)
}

type statsSnapshot struct {
	Agents struct {
		Total  int `json:"total"`
		Online int `json:"online"`
	} `json:"agents"`
	Tasks struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
		InFlight  int `json:"in_flight"`
	} `json:"tasks"`
	Visits        int64 `json:"visits"`
	UptimeSeconds int64 `json:"uptime_seconds"`
}

func (b *Broker) collectStats() statsSnapshot {
	var s statsSnapshot
	total, online, _ := b.Store.CountAgents()
	s.Agents.Total, s.Agents.Online = total, online
	tt, tc, _ := b.Store.CountTasks()
	s.Tasks.Total, s.Tasks.Completed = tt, tc
	s.Tasks.InFlight = tt - tc
	if v, err := b.Store.GetStat("visits"); err == nil {
		s.Visits = v
	}
	s.UptimeSeconds = int64(time.Since(startedAt).Seconds())
	return s
}

// handleAgentsPage renders a human-readable HTML table of every registered
// agent. It honors X-Forwarded-Prefix so links work behind a reverse proxy.
func (b *Broker) handleAgentsPage(c *gin.Context) {
	agents, err := b.Store.ListAgents(false)
	if err != nil {
		c.String(http.StatusInternalServerError, "list agents: %v", err)
		return
	}
	// Sort: online first, then by most-recent last_seen.
	sort.SliceStable(agents, func(i, j int) bool {
		if agents[i].OnlineRT != agents[j].OnlineRT {
			return agents[i].OnlineRT
		}
		return agents[i].LastSeen.After(agents[j].LastSeen)
	})

	base := b.prefixPath(c)
	stats := b.collectStats()

	var rows strings.Builder
	for _, a := range agents {
		skills := cardSkills(a.CardJSON)
		skillsHTML := "<span class=muted>—</span>"
		if len(skills) > 0 {
			var chips []string
			for _, sk := range skills {
				chips = append(chips, fmt.Sprintf(`<span class="chip" title=%q>%s</span>`,
					html.EscapeString(sk.Desc), html.EscapeString(sk.Display())))
			}
			skillsHTML = strings.Join(chips, " ")
		}

		status := `<span class="dot off"></span>offline`
		if a.OnlineRT {
			status = `<span class="dot on"></span>online`
		}

		desc := cardDescription(a.CardJSON)

		fmt.Fprintf(&rows, `
<tr>
  <td><div class="name">%s</div><div class="muted small">%s</div></td>
  <td><code class="inline">%s</code></td>
  <td>%s</td>
  <td><span class="mode">%s</span></td>
  <td>%s</td>
  <td class="muted small">%s</td>
  <td><a class="mini" href="%s/agents/%s/.well-known/agent-card.json" target="_blank">card</a>
      <a class="mini" href="%s/agents/%s/a2a" title="POST JSON-RPC here">a2a</a></td>
</tr>`,
			html.EscapeString(a.Name),
			html.EscapeString(desc),
			html.EscapeString(a.ID),
			status,
			html.EscapeString(a.Mode),
			skillsHTML,
			a.LastSeen.UTC().Format("2006-01-02 15:04 UTC"),
			base, a.ID, base, a.ID,
		)
	}

	if len(agents) == 0 {
		rows.WriteString(`<tr><td colspan="7" class="empty">还没有 Agent 注册 · No agents registered yet.</td></tr>`)
	}

	page := fmt.Sprintf(agentsPageTemplate, base, base,
		stats.Agents.Online, stats.Agents.Total, stats.Tasks.Total, stats.Visits,
		rows.String(), base)

	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(page))
}

type skillRow struct {
	ID   string
	Name string
	Tags []string
	Desc string
}

func (s skillRow) Display() string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

func cardSkills(card json.RawMessage) []skillRow {
	var c struct {
		Skills []struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Tags        []string `json:"tags"`
		} `json:"skills"`
	}
	_ = json.Unmarshal(card, &c)
	var out []skillRow
	for _, s := range c.Skills {
		out = append(out, skillRow{ID: s.ID, Name: s.Name, Desc: s.Description, Tags: s.Tags})
	}
	return out
}

func cardDescription(card json.RawMessage) string {
	var c struct {
		Description string `json:"description"`
	}
	_ = json.Unmarshal(card, &c)
	return c.Description
}

const agentsPageTemplate = `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>注册 Agent · A2A Broker</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="30">
<style>
  :root { --fg:#0f172a; --muted:#64748b; --bg:#fafafa; --card:#fff; --accent:#0f62fe; --line:#e2e8f0; --good:#16a34a }
  * { box-sizing:border-box }
  html,body { margin:0; background:var(--bg); color:var(--fg);
    font:15px/1.55 "PingFang SC","Microsoft YaHei",-apple-system,"SF Pro Text",Segoe UI,Roboto,sans-serif }
  main { max-width: 1100px; margin: 2rem auto; padding: 0 1.2rem }
  h1 { font-size: 1.6rem; margin:0 0 .4rem; display:flex; align-items:center; gap:.6rem }
  .pill { display:inline-block; background:#eef2ff; color:#3730a3; font-size:12px; padding:.15rem .5rem; border-radius:999px }
  .row { display:flex; gap:.5rem; flex-wrap:wrap; margin:.9rem 0 }
  a.btn { background: var(--accent); color:white; padding:.45rem .8rem; border-radius:7px; text-decoration:none; font-weight:600; font-size:.9rem }
  a.btn.secondary { background:transparent; color: var(--accent); border: 1px solid var(--accent) }
  .stats { display:grid; grid-template-columns: repeat(4,minmax(0,1fr)); gap:.6rem; margin:1.2rem 0 }
  @media (max-width:640px) { .stats { grid-template-columns: repeat(2,1fr) } }
  .stat { background: var(--card); border:1px solid var(--line); border-radius:10px; padding:.8rem .9rem }
  .stat .n { font-size:1.6rem; font-weight:700; line-height:1.1 }
  .stat .k { color: var(--muted); font-size: .85rem; margin-top:.15rem }
  table.agents { width:100%%; border-collapse: collapse; background: var(--card); border:1px solid var(--line); border-radius: 10px; overflow:hidden }
  table.agents th, table.agents td { padding: .55rem .75rem; text-align:left; font-size:.9rem; vertical-align: top; border-bottom:1px solid var(--line) }
  table.agents th { background:#f1f5f9; color:#334155; font-weight:600; text-transform: none; letter-spacing:0; font-size:.78rem }
  table.agents tr:last-child td { border-bottom: none }
  .name { font-weight:600 }
  .muted { color: var(--muted) }
  .small { font-size: .8rem }
  .dot { display:inline-block; width:.5rem; height:.5rem; border-radius:50%%; margin-right:.3rem; vertical-align: middle }
  .dot.on { background: var(--good); box-shadow:0 0 0 3px rgba(22,163,74,.15) }
  .dot.off { background:#94a3b8 }
  .chip { display:inline-block; background:#eef2f7; color:#0f172a; font-size:.75rem; padding:.1rem .45rem; border-radius: 999px; margin: 1px 0 }
  .mode { background: #fef3c7; color:#92400e; font-size:.75rem; padding:.1rem .45rem; border-radius: 4px }
  a.mini { color: var(--accent); text-decoration: none; font-size: .78rem; margin-right:.35rem }
  a.mini:hover { text-decoration: underline }
  code, code.inline { font-family: ui-monospace,SFMono-Regular,Menlo,monospace; background:#eef2f7; padding:.1rem .3rem; border-radius:4px; font-size:.78rem }
  .empty { padding: 2rem; text-align:center; color: var(--muted) }
  footer { color: var(--muted); font-size: .82rem; margin: 2rem 0 2rem; text-align:center }
</style>
</head>
<body>
<main>

<h1>注册 Agent 目录 <span class="pill">A2A Broker</span></h1>
<p class="muted">每 30 秒自动刷新 · <a href="%s/stats">查看 JSON stats</a> · <a href="%s/">返回首页</a></p>

<div class="stats">
  <div class="stat"><div class="n">%d</div><div class="k">在线 / Online</div></div>
  <div class="stat"><div class="n">%d</div><div class="k">累计注册 / Total agents</div></div>
  <div class="stat"><div class="n">%d</div><div class="k">任务总数 / Tasks</div></div>
  <div class="stat"><div class="n">%d</div><div class="k">页面访问 / Page visits</div></div>
</div>

<table class="agents">
  <thead>
    <tr>
      <th>名称 / Name</th>
      <th>agent_id</th>
      <th>状态 / Status</th>
      <th>模式 / Mode</th>
      <th>能力 / Skills</th>
      <th>最近活跃 / Last seen</th>
      <th>链接</th>
    </tr>
  </thead>
  <tbody>
%s
  </tbody>
</table>

<footer>任何人都可以
<a href="%s/#register">注册一个 Agent</a> · Anyone can register</footer>

</main>
</body>
</html>
`
