{{- /*
  cluster.tpl — the Cluster Map (§4, "Meet the Cluster" style).

  A static shell: the SVG scene, legend, zoom controls, and the floating
  node info windows are all driven by /assets/cluster.js polling
  /cluster/topology.json. The map is full-bleed — it fills everything
  under the nav (style.css `main:has(> .cmap)`), so this content block
  deliberately has no heading or copy of its own. Node windows show
  shard-level detail only — never key or blob names.

  Data: none (the page carries no server-rendered cluster state).
*/ -}}
{{define "content"}}
<div id="cmap" class="cmap">
  <div class="cmap-grid" id="cmap-grid"></div>
  <svg id="cmap-svg"></svg>

  <div class="cmap-legend">
    <b>reading the map</b><br>
    ◆ metadata voter <span class="cmap-dim">(gold = leader)</span><br>
    ★n leads n raft groups<br>
    n⛁ shard replicas · n▦ blob chunks<br>
    <span class="cmap-dim">╌╌ metadata mesh — leader→follower links solid<br>
    drag nodes · wheel zooms · click nodes for details</span>
  </div>

  <div class="cmap-zoom">
    <button type="button" id="cmap-zin" title="zoom in">+</button>
    <button type="button" id="cmap-zout" title="zoom out">−</button>
    <button type="button" id="cmap-fit" title="fit map">⤢</button>
  </div>

  <div class="cmap-status" id="cmap-status">connecting…</div>
</div>
<script src="/assets/cluster.js"></script>
{{end}}
