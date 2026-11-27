//go:build ignore

// genchart renders the README SVG performance charts:
//   - docs/assets/bench-compare.svg (execution time + 256-VM Go GC pause)
//   - docs/assets/bench-multivm.svg (high-density 32..512-VM Go GC scaling + per-VM memory & cold start)
//
// Inputs are JSON summaries produced by benchreport.go -json and memcmp.go -json.
package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type reportSummary struct {
	Baseline      string                        `json:"baseline"`
	Rounds        int                           `json:"rounds"`
	CommonCount   int                           `json:"common_count"`
	Engines       []string                      `json:"engines"`
	CommonGeomean map[string]float64            `json:"common_geomean"`
	AllGeomean    map[string]float64            `json:"all_geomean"`
	SuiteGeomean  map[string]map[string]float64 `json:"suite_geomean"`
}

type scalePoint struct {
	VMs           int     `json:"vms"`
	ActiveHeapKiB float64 `json:"active_heap_kib"`
	ActiveRSSKiB  float64 `json:"active_rss_kib"`
	ActiveGCMs    float64 `json:"active_gc_ms"`
}

type memResult struct {
	Engine        string       `json:"engine"`
	VMs           int          `json:"vms"`
	NewStateUs    float64      `json:"new_state_us"`
	IdleHeapKiB   float64      `json:"idle_heap_kib"`
	IdleRSSKiB    float64      `json:"idle_rss_kib"`
	HasRSS        bool         `json:"has_rss"`
	IdleGCMs      float64      `json:"idle_gc_ms"`
	ActiveHeapKiB float64      `json:"active_heap_kib"`
	ActiveRSSKiB  float64      `json:"active_rss_kib"`
	ActiveGCMs    float64      `json:"active_gc_ms"`
	Scale         []scalePoint `json:"scale,omitempty"`
}

type leftRow struct {
	label string
	val   float64
	kind  string // "ref", "moe", "other"
}

type rightRow struct {
	label string
	val   float64
	note  string
	kind  string // "ref", "moe", "other"
}

func niceAxisMax(v float64, step float64) float64 {
	if v <= step {
		return step
	}
	return math.Ceil((v*1.08)/step) * step
}

func formatMemSize(kib float64) string {
	if kib >= 1024 {
		return fmt.Sprintf("%.1f MiB", kib/1024.0)
	}
	return fmt.Sprintf("%.0f KiB", kib)
}

func formatMs(ms float64) string {
	if ms < 10 {
		return fmt.Sprintf("%.2f ms", ms)
	}
	return fmt.Sprintf("%.1f ms", ms)
}

func formatMsShort(ms float64) string {
	if ms < 10 {
		return fmt.Sprintf("%.1fms", ms)
	}
	return fmt.Sprintf("%.0fms", ms)
}

func formatUs(us float64) string {
	if us >= 1000 {
		return fmt.Sprintf("%.1fms", us/1000.0)
	}
	return fmt.Sprintf("%.0fµs", us)
}

func writeBadge(sb *strings.Builder, badge string) {
	if badge == "" {
		return
	}
	// Estimate pill width from monospace character count.
	w := float64(len(badge))*6.5 + 18.0
	if w < 108 {
		w = 108
	}
	x := 936.0 - w
	fmt.Fprintf(sb, `  <rect x="%.1f" y="14" width="%.1f" height="22" rx="6" class="badge-bg"/>`+"\n", x, w)
	fmt.Fprintf(sb, `  <text x="%.1f" y="29" class="badge-txt">%s</text>`+"\n", x+w/2, html.EscapeString(badge))
}

const commonCSS = `    <style>
      .bg { fill: #ffffff; }
      .border { fill: none; stroke: #e2e8f0; stroke-width: 1; }
      .badge-bg { fill: #f8fafc; stroke: #e2e8f0; stroke-width: 1; }
      .badge-txt { font: 500 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: #475569; text-anchor: middle; }
      .title { font: 700 16px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #0f172a; }
      .subtitle { font: 400 11.5px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #475569; }
      .panel-title { font: 700 13px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #0f172a; }
      .panel-sub { font: 400 11px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #64748b; }
      .label { font: 500 12px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #334155; text-anchor: end; }
      .label-hi { font: 700 12px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #047857; text-anchor: end; }
      .val { font: 600 11.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: #334155; }
      .val-hi { font: 700 12px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: #047857; }
      .end-lbl { font: 500 10.5px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #475569; }
      .end-lbl-hi { font: 700 11px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #047857; }
      .grid { stroke: #f1f5f9; stroke-width: 1; }
      .axis { stroke: #cbd5e1; stroke-width: 1; }
      .refline { stroke: #64748b; stroke-width: 1; stroke-dasharray: 3 3; }
      .leader { stroke: #cbd5e1; stroke-width: 1; fill: none; }
      .tick { font: 400 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: #64748b; text-anchor: middle; }
      .tick-y { font: 400 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: #64748b; text-anchor: end; }
      .foot { font: 400 10.5px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #64748b; }
      .bar-moe-jit { fill: #059669; }
      .bar-moe { fill: #10b981; }
      .bar-other { fill: #64748b; }
      .bar-ref { fill: url(#hatch); stroke: #94a3b8; stroke-width: 1; }
    </style>`

func renderCompareSVG(bench reportSummary, mems []memResult, subtitle, badge string) (string, error) {
	if _, ok := bench.CommonGeomean["C"]; !ok || bench.CommonCount == 0 {
		return "", fmt.Errorf("bench json missing C baseline common_geomean")
	}

	leftNames := map[string]struct {
		label string
		kind  string
	}{
		"C-O3":          {"C Lua 5.5.1 -O3 (ref)", "ref"},
		"C":             {"C Lua 5.5.1 -O2 (ref)", "ref"},
		"MoeLua-JIT":    {"MoeLua 5.5 (JIT)", "moe-jit"},
		"Apogee-JIT":    {"Apogee v1.2.1 (JIT)", "other"},
		"MoeLua":        {"MoeLua 5.5 (noJIT)", "moe"},
		"Apogee-Interp": {"Apogee v1.2.1 (noJIT)", "other"},
		"CCGO":          {"modernc/lua (CCGO)", "other"},
		"ArnodelGolua":  {"arnodel/golua (PGO)", "other"},
		"GopherLua":     {"yuin/gopher-lua", "other"},
		"GoLua":         {"Shopify/go-lua", "other"},
		"DCLua":         {"DCLua (milochristiansen)", "other"},
	}

	var leftRows []leftRow
	for key, meta := range leftNames {
		if v, ok := bench.CommonGeomean[key]; ok {
			leftRows = append(leftRows, leftRow{label: meta.label, val: v, kind: meta.kind})
		}
	}
	slices.SortFunc(leftRows, func(a, b leftRow) int {
		if c := cmp.Compare(a.val, b.val); c != 0 {
			return c
		}
		return cmp.Compare(a.label, b.label)
	})

	rightLabels := map[string]struct {
		label string
		kind  string
	}{
		"MoeLua-JIT":     {"MoeLua 5.5 (JIT)", "moe-jit"},
		"MoeLua":         {"MoeLua 5.5 (noJIT)", "moe"},
		"CCGO":           {"modernc/lua (mmap)", "other"},
		"arnodel/golua":  {"arnodel/golua", "other"},
		"Apogee-JIT":     {"Apogee v1.2.1 (JIT)", "other"},
		"Apogee-noJIT":   {"Apogee v1.2.1 (noJIT)", "other"},
		"DCLua":          {"DCLua", "other"},
		"Shopify/go-lua": {"Shopify/go-lua", "other"},
		"GopherLua":      {"yuin/gopher-lua", "other"},
	}
	var rightRows []rightRow
	var moeIdleKiB float64
	var ccgoActHeapKiB float64
	hasCCGO := false
	hasAnyRSS := false
	vmCount := 256
	for _, r := range mems {
		if r.VMs > 0 {
			vmCount = r.VMs
		}
		if r.HasRSS && r.ActiveRSSKiB > 0 {
			hasAnyRSS = true
		}
		if r.Engine == "MoeLua" {
			if r.HasRSS && r.IdleRSSKiB > 0 {
				moeIdleKiB = r.IdleRSSKiB
			} else {
				moeIdleKiB = r.IdleHeapKiB
			}
		}
		if r.Engine == "CCGO" {
			hasCCGO = true
			ccgoActHeapKiB = r.ActiveHeapKiB
		}
		meta, ok := rightLabels[r.Engine]
		if !ok {
			continue
		}
		memVal := r.ActiveHeapKiB
		if r.HasRSS && r.ActiveRSSKiB > 0 {
			memVal = r.ActiveRSSKiB
		}
		rightRows = append(rightRows, rightRow{
			label: meta.label,
			val:   r.ActiveGCMs,
			note:  fmt.Sprintf("%s (%s)", formatMs(r.ActiveGCMs), formatMemSize(memVal)),
			kind:  meta.kind,
		})
	}
	if len(rightRows) == 0 {
		return "", fmt.Errorf("no recognized engines in mem json")
	}
	slices.SortFunc(rightRows, func(a, b rightRow) int {
		if c := cmp.Compare(a.val, b.val); c != 0 {
			return c
		}
		return cmp.Compare(a.label, b.label)
	})

	maxLeftVal := 2.0
	for _, r := range leftRows {
		if r.val > maxLeftVal {
			maxLeftVal = r.val
		}
	}
	leftStep := 2.0
	if maxLeftVal > 12 {
		leftStep = 4.0
	}
	leftAxisMax := niceAxisMax(maxLeftVal, leftStep)
	if leftAxisMax < 10 && maxLeftVal >= 8.5 {
		leftAxisMax = 10
	}

	maxRightMs := 10.0
	for _, r := range rightRows {
		if r.val > maxRightMs {
			maxRightMs = r.val
		}
	}
	rightStep := 50.0
	if maxRightMs <= 60 {
		rightStep = 20.0
	} else if maxRightMs > 250 {
		rightStep = 100.0
	}
	rightAxisMax := niceAxisMax(maxRightMs, rightStep)

	const (
		svgW       = 960.0
		leftX0     = 165.0
		leftPlotW  = 260.0
		rightX0    = 648.0
		rightPlotW = 210.0
		plotTop    = 108.0
		plotBottom = 378.0
		barH       = 15.0
	)

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" width="960" height="450" viewBox="0 0 960 450" role="img" aria-label="MoeLua 5.5 cross-engine benchmark: %d-program execution time relative to C Lua 5.5.1 -O2 and %d-VM Go GC pause">
  <defs>
    <pattern id="hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
      <rect width="6" height="6" fill="#e2e8f0"/>
      <line x1="0" y1="0" x2="0" y2="6" stroke="#94a3b8" stroke-width="2"/>
    </pattern>
%s
  </defs>

  <rect x="0.5" y="0.5" width="959" height="449" rx="10" class="bg"/>
  <rect x="0.5" y="0.5" width="959" height="449" rx="10" class="border"/>

  <!-- Header -->
  <text x="24" y="30" class="title">MoeLua 5.5 vs Go Lua Engines &amp; C Lua 5.5.1</text>
  <text x="24" y="48" class="subtitle">%s</text>
`, bench.CommonCount, vmCount, commonCSS, html.EscapeString(subtitle))
	writeBadge(&sb, badge)

	fmt.Fprintf(&sb, `
  <!-- Divider -->
  <line x1="490" y1="68" x2="490" y2="404" stroke="#e2e8f0" stroke-width="1"/>

  <!-- LEFT PANEL -->
  <text x="24" y="78" class="panel-title">Execution Time (%d-Program Geomean)</text>
  <text x="24" y="94" class="panel-sub">Relative to C Lua 5.5.1 -O2 (1.00x) · AWFY + Benchmarks Game + SciMark + Own</text>

`, bench.CommonCount)

	fmt.Fprintf(&sb, "  <line x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" class=\"axis\"/>\n", leftX0, plotTop, leftX0, plotBottom)
	refX := leftX0 + (1.0/leftAxisMax)*leftPlotW
	fmt.Fprintf(&sb, "  <line x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" class=\"refline\"/>\n", refX, plotTop, refX, plotBottom)
	fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"393\" class=\"tick\">0x</text>\n", leftX0)
	if refX-leftX0 >= 18 {
		fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"393\" class=\"tick\">1x</text>\n", refX)
	}
	for tick := leftStep; tick <= leftAxisMax+1e-6; tick += leftStep {
		x := leftX0 + (tick/leftAxisMax)*leftPlotW
		fmt.Fprintf(&sb, "  <line x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" class=\"grid\"/>\n", x, plotTop, x, plotBottom)
		if x-refX >= 18 {
			fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"393\" class=\"tick\">%.0fx</text>\n", x, tick)
		}
	}

	leftRowH := (plotBottom - plotTop - 6) / float64(max(len(leftRows), 1))
	if leftRowH > 28 {
		leftRowH = 28
	}
	y := plotTop + 6
	for _, r := range leftRows {
		w := max((r.val/leftAxisMax)*leftPlotW, 2.0)
		lblCls := "label"
		valCls := "val"
		if r.kind == "moe" || r.kind == "moe-jit" {
			lblCls = "label-hi"
			valCls = "val-hi"
		}
		barCls := map[string]string{"moe-jit": "bar-moe-jit", "moe": "bar-moe", "ref": "bar-ref", "other": "bar-other"}[r.kind]
		fmt.Fprintf(&sb, "  <text x=\"157\" y=\"%.1f\" class=\"%s\">%s</text>\n", y+12, lblCls, html.EscapeString(r.label))
		fmt.Fprintf(&sb, "  <rect x=\"%.1f\" y=\"%.1f\" width=\"%.1f\" height=\"%.1f\" rx=\"3\" class=\"%s\"/>\n", leftX0, y, w, barH, barCls)
		valX := leftX0 + w + 6
		if leftX0+w < refX+2 && valX < refX+5 {
			valX = refX + 5
		}
		fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"%.1f\" class=\"%s\">%.2fx</text>\n", valX, y+12, valCls, r.val)
		y += leftRowH
	}

	fmt.Fprintf(&sb, `
  <!-- RIGHT PANEL -->
  <text x="512" y="78" class="panel-title">Go GC Pause with %d Active VMs</text>
  <text x="512" y="94" class="panel-sub">runtime.GC() median (ms) · 2,047 tables/VM (%dK live Lua tables) · isolated process</text>

`, vmCount, (vmCount*2047)/1000)

	fmt.Fprintf(&sb, "  <line x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" class=\"axis\"/>\n", rightX0, plotTop, rightX0, plotBottom)
	fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"393\" class=\"tick\">0</text>\n", rightX0)
	for tick := rightStep; tick <= rightAxisMax+1e-6; tick += rightStep {
		x := rightX0 + (tick/rightAxisMax)*rightPlotW
		fmt.Fprintf(&sb, "  <line x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" class=\"grid\"/>\n", x, plotTop, x, plotBottom)
		fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"393\" class=\"tick\">%.0fms</text>\n", x, tick)
	}

	rightRowH := (plotBottom - plotTop - 10) / float64(max(len(rightRows), 1))
	if rightRowH > 34 {
		rightRowH = 34
	}
	y = plotTop + 10
	for _, r := range rightRows {
		w := max((r.val/rightAxisMax)*rightPlotW, 3.0)
		lblCls := "label"
		valCls := "val"
		if r.kind == "moe" || r.kind == "moe-jit" {
			lblCls = "label-hi"
			valCls = "val-hi"
		}
		barCls := map[string]string{"moe-jit": "bar-moe-jit", "moe": "bar-moe", "ref": "bar-ref", "other": "bar-other"}[r.kind]
		fmt.Fprintf(&sb, "  <text x=\"640\" y=\"%.1f\" class=\"%s\">%s</text>\n", y+12, lblCls, html.EscapeString(r.label))
		fmt.Fprintf(&sb, "  <rect x=\"%.1f\" y=\"%.1f\" width=\"%.1f\" height=\"%.1f\" rx=\"3\" class=\"%s\"/>\n", rightX0, y, w, barH, barCls)
		estTextW := float64(len(r.note)) * 7.0
		if rightX0+w+6+estTextW > svgW-14 && w > estTextW+12 {
			fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"%.1f\" class=\"val\" style=\"fill:#ffffff;text-anchor:end\">%s</text>\n", rightX0+w-6, y+12, html.EscapeString(r.note))
		} else {
			fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"%.1f\" class=\"%s\">%s</text>\n", rightX0+w+6, y+12, valCls, html.EscapeString(r.note))
		}
		y += rightRowH
	}

	leftFoot := fmt.Sprintf("Left: median of %d rounds via bench/compare (32/32 byte-exact output vs C Lua 5.5.1).", max(bench.Rounds, 1))
	if v, ok := bench.CommonGeomean["LuaJIT"]; ok {
		leftFoot += fmt.Sprintf(" LuaJIT on the same suite: %.2fx (see docs/benchmarks.md).", v)
	}
	memMetric := "active Go heap/VM"
	if hasAnyRSS {
		memMetric = "active OS RSS/VM"
	}
	var rightFootParts []string
	rightFootParts = append(rightFootParts, fmt.Sprintf("Right: %s in parentheses.", memMetric))
	if hasCCGO {
		rightFootParts = append(rightFootParts, fmt.Sprintf("modernc/lua uses off-heap mmap (%.1f KiB Go heap).", ccgoActHeapKiB))
	}
	if moeIdleKiB > 0 {
		rightFootParts = append(rightFootParts, fmt.Sprintf("MoeLua uses noscan Arenas + Lua 5.5 GC (%.0f KiB idle/VM).", moeIdleKiB))
	}
	rightFoot := strings.Join(rightFootParts, " ")

	fmt.Fprintf(&sb, `
  <!-- Footer notes -->
  <text x="24" y="422" class="foot">%s</text>
  <text x="24" y="438" class="foot">%s</text>
</svg>
`, html.EscapeString(leftFoot), html.EscapeString(rightFoot))

	return sb.String(), nil
}

type curveSpec struct {
	engine   string
	shortLbl string
	kind     string // "moe-jit", "moe", "other"
	isDashed bool
	points   []scalePoint
	endY     float64
	labelY   float64
	endMs    float64
}

func resolveLabelY(curves []*curveSpec, topY, botY, minGap float64) {
	if len(curves) == 0 {
		return
	}
	slices.SortFunc(curves, func(a, b *curveSpec) int {
		if c := cmp.Compare(a.endY, b.endY); c != 0 {
			return c
		}
		return cmp.Compare(a.shortLbl, b.shortLbl)
	})
	for _, c := range curves {
		c.labelY = math.Max(topY, math.Min(botY, c.endY))
	}
	// Forward pass: push overlapping labels down.
	for i := 1; i < len(curves); i++ {
		if curves[i].labelY < curves[i-1].labelY+minGap {
			curves[i].labelY = curves[i-1].labelY + minGap
		}
	}
	// If bottom overflowed, shift backward pass up.
	if over := curves[len(curves)-1].labelY - botY; over > 0 {
		curves[len(curves)-1].labelY = botY
		for i := len(curves) - 2; i >= 0; i-- {
			if curves[i].labelY > curves[i+1].labelY-minGap {
				curves[i].labelY = curves[i+1].labelY - minGap
			}
		}
	}
	// Clamp top if still tight.
	if curves[0].labelY < topY {
		curves[0].labelY = topY
		for i := 1; i < len(curves); i++ {
			if curves[i].labelY < curves[i-1].labelY+minGap {
				curves[i].labelY = curves[i-1].labelY + minGap
			}
		}
	}
}

func renderMultiVMSVG(mems []memResult, subtitle, badge string) (string, error) {
	metaMap := map[string]struct {
		fullLbl  string
		shortLbl string
		kind     string // "ref", "moe", "other"
		hasGoGC  bool
		isDashed bool
	}{
		"C":              {"C Lua 5.5.1 (ref)", "C Lua", "ref", false, false},
		"MoeLua-JIT":     {"MoeLua 5.5 (JIT)", "MoeLua 5.5 (JIT)", "moe-jit", true, false},
		"MoeLua":         {"MoeLua 5.5 (noJIT)", "MoeLua 5.5 (noJIT)", "moe", true, false},
		"CCGO":           {"modernc/lua (mmap)", "modernc/lua", "other", true, true},
		"arnodel/golua":  {"arnodel/golua", "golua", "other", true, false},
		"Apogee-JIT":     {"Apogee v1.2.1 (JIT)", "Apogee JIT", "other", true, false},
		"Apogee-noJIT":   {"Apogee v1.2.1 (noJIT)", "Apogee noJIT", "other", true, false},
		"DCLua":          {"DCLua", "DCLua", "other", true, false},
		"Shopify/go-lua": {"Shopify/go-lua", "go-lua", "other", true, false},
		"GopherLua":      {"yuin/gopher-lua", "gopher-lua", "other", true, false},
	}

	var curves []*curveSpec
	var rightRows []rightRow
	var xTicks []int
	maxVMs := 0
	minVMs := 0
	maxMs := 10.0
	hasAnyRSS := false
	var moeIdleKiB, moeIdleGC, apogeeIdleKiB, apogeeIdleGC float64
	idleVMs := 256

	for _, r := range mems {
		if r.VMs > 0 {
			idleVMs = r.VMs
		}
		meta, ok := metaMap[r.Engine]
		if !ok || len(r.Scale) == 0 {
			continue
		}
		pts := slices.Clone(r.Scale)
		slices.SortFunc(pts, func(a, b scalePoint) int { return cmp.Compare(a.VMs, b.VMs) })
		last := pts[len(pts)-1]
		if meta.hasGoGC {
			if last.VMs > maxVMs {
				maxVMs = last.VMs
			}
			if minVMs == 0 || pts[0].VMs < minVMs {
				minVMs = pts[0].VMs
			}
			for _, pt := range pts {
				if pt.ActiveGCMs > maxMs {
					maxMs = pt.ActiveGCMs
				}
				if !slices.Contains(xTicks, pt.VMs) {
					xTicks = append(xTicks, pt.VMs)
				}
			}
			curves = append(curves, &curveSpec{
				engine:   r.Engine,
				shortLbl: meta.shortLbl,
				kind:     meta.kind,
				isDashed: meta.isDashed,
				points:   pts,
				endMs:    last.ActiveGCMs,
			})
		}
		if r.HasRSS && last.ActiveRSSKiB > 0 {
			hasAnyRSS = true
		}
		if r.Engine == "MoeLua" {
			moeIdleGC = r.IdleGCMs
			if r.HasRSS && r.IdleRSSKiB > 0 {
				moeIdleKiB = r.IdleRSSKiB
			} else {
				moeIdleKiB = r.IdleHeapKiB
			}
		}
		if r.Engine == "Apogee-JIT" {
			apogeeIdleGC = r.IdleGCMs
			if r.HasRSS && r.IdleRSSKiB > 0 {
				apogeeIdleKiB = r.IdleRSSKiB
			} else {
				apogeeIdleKiB = r.IdleHeapKiB
			}
		}
		memVal := last.ActiveHeapKiB
		if r.HasRSS && last.ActiveRSSKiB > 0 {
			memVal = last.ActiveRSSKiB
		} else if meta.kind == "ref" {
			// Skip C reference bar on platforms without OS RSS measurement (e.g. Windows).
			continue
		}
		note := formatMemSize(memVal)
		if r.NewStateUs > 0 {
			note = fmt.Sprintf("%s · %s", formatMemSize(memVal), formatUs(r.NewStateUs))
		}
		rightRows = append(rightRows, rightRow{
			label: meta.fullLbl,
			val:   memVal,
			note:  note,
			kind:  meta.kind,
		})
	}
	if len(curves) == 0 || maxVMs == 0 {
		return "", fmt.Errorf("mem json missing scale data for high-density multi-VM chart")
	}
	slices.Sort(xTicks)
	slices.SortFunc(rightRows, func(a, b rightRow) int {
		if c := cmp.Compare(a.val, b.val); c != 0 {
			return c
		}
		return cmp.Compare(a.label, b.label)
	})

	yStep := 100.0
	if maxMs <= 120 {
		yStep = 25.0
	} else if maxMs <= 250 {
		yStep = 50.0
	} else if maxMs > 1200 {
		yStep = 400.0
	} else if maxMs > 600 {
		yStep = 200.0
	}
	yAxisMax := niceAxisMax(maxMs, yStep)

	maxMemKiB := 512.0
	for _, r := range rightRows {
		if r.val > maxMemKiB {
			maxMemKiB = r.val
		}
	}
	memStep := 512.0
	if maxMemKiB <= 1024 {
		memStep = 256.0
	} else if maxMemKiB > 3072 {
		memStep = 1024.0
	}
	memAxisMax := niceAxisMax(maxMemKiB, memStep)

	const (
		svgW       = 960.0
		lX0        = 64.0
		lPlotW     = 244.0
		rX0        = 648.0
		rPlotW     = 200.0
		plotTop    = 112.0
		plotBottom = 376.0
		barH       = 15.0
	)
	lPlotH := plotBottom - plotTop

	xOf := func(vms int) float64 {
		return lX0 + (float64(vms)/float64(maxVMs))*lPlotW
	}
	yOf := func(ms float64) float64 {
		return plotBottom - (ms/yAxisMax)*lPlotH
	}
	for _, c := range curves {
		c.endY = yOf(c.endMs)
	}
	resolveLabelY(curves, plotTop+6, plotBottom-2, 16.0)

	totalTablesM := float64(maxVMs*2047) / 1e6
	memKindTitle := "Go Heap/VM"
	if hasAnyRSS {
		memKindTitle = "OS RSS/VM"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" width="960" height="450" viewBox="0 0 960 450" role="img" aria-label="MoeLua 5.5 high-density multi-VM scalability: Go GC pause from %d to %d active VMs and per-VM memory and cold start">
  <defs>
    <pattern id="hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
      <rect width="6" height="6" fill="#e2e8f0"/>
      <line x1="0" y1="0" x2="0" y2="6" stroke="#94a3b8" stroke-width="2"/>
    </pattern>
%s
  </defs>

  <rect x="0.5" y="0.5" width="959" height="449" rx="10" class="bg"/>
  <rect x="0.5" y="0.5" width="959" height="449" rx="10" class="border"/>

  <!-- Header -->
  <text x="24" y="30" class="title">High-Density Active Multi-VM Scalability (%d to %d Active VMs)</text>
  <text x="24" y="48" class="subtitle">%s</text>
`, minVMs, maxVMs, commonCSS, minVMs, maxVMs, html.EscapeString(subtitle))
	writeBadge(&sb, badge)

	fmt.Fprintf(&sb, `
  <!-- Divider -->
  <line x1="490" y1="68" x2="490" y2="404" stroke="#e2e8f0" stroke-width="1"/>

  <!-- LEFT PANEL: GC Scaling Line Chart -->
  <text x="24" y="78" class="panel-title">Go GC Pause Scaling by Active VM Count</text>
  <text x="24" y="94" class="panel-sub">runtime.GC() median (ms) · 2,047 tables/VM (up to %.2fM live Lua tables)</text>

`, totalTablesM)

	// Horizontal gridlines + Y ticks
	for tick := 0.0; tick <= yAxisMax+1e-6; tick += yStep {
		yy := yOf(tick)
		cls := "grid"
		if tick == 0 {
			cls = "axis"
		}
		fmt.Fprintf(&sb, "  <line x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" class=\"%s\"/>\n", lX0, yy, lX0+lPlotW, yy, cls)
		lbl := "0"
		if tick > 0 {
			lbl = fmt.Sprintf("%.0fms", tick)
		}
		fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"%.1f\" class=\"tick-y\">%s</text>\n", lX0-6, yy+3.5, lbl)
	}
	// Y axis line + vertical gridlines for X ticks
	fmt.Fprintf(&sb, "  <line x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" class=\"axis\"/>\n", lX0, plotTop, lX0, plotBottom)
	for _, vt := range xTicks {
		xx := xOf(vt)
		fmt.Fprintf(&sb, "  <line x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" class=\"grid\"/>\n", xx, plotTop, xx, plotBottom)
		fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"393\" class=\"tick\">%d</text>\n", xx, vt)
	}
	fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"406\" class=\"tick\" style=\"fill:#94a3b8\">active VMs</text>\n", lX0+lPlotW/2)

	// Draw non-MoeLua curves first, MoeLua last on top.
	isMoeCurve := func(c *curveSpec) bool { return c.kind == "moe" || c.kind == "moe-jit" }
	drawCurve := func(c *curveSpec) {
		var pts []string
		for _, pt := range c.points {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", xOf(pt.VMs), yOf(pt.ActiveGCMs)))
		}
		stroke := "#94a3b8"
		sw := 1.7
		rDot := 2.4
		dash := ""
		if c.isDashed {
			dash = ` stroke-dasharray="4 3"`
		}
		if c.kind == "moe-jit" {
			stroke = "#059669"
			sw = 3.0
			rDot = 3.6
		} else if c.kind == "moe" {
			stroke = "#10b981"
			sw = 2.6
			rDot = 3.2
		}
		fmt.Fprintf(&sb, "  <polyline fill=\"none\" stroke=\"%s\" stroke-width=\"%.1f\"%s stroke-linecap=\"round\" stroke-linejoin=\"round\" points=\"%s\"/>\n",
			stroke, sw, dash, strings.Join(pts, " "))
		for _, pt := range c.points {
			fmt.Fprintf(&sb, "  <circle cx=\"%.1f\" cy=\"%.1f\" r=\"%.1f\" fill=\"%s\"/>\n", xOf(pt.VMs), yOf(pt.ActiveGCMs), rDot, stroke)
		}
		endX := lX0 + lPlotW
		lblX := endX + 12
		if math.Abs(c.labelY-c.endY) > 2.5 {
			fmt.Fprintf(&sb, "  <polyline class=\"leader\" points=\"%.1f,%.1f %.1f,%.1f %.1f,%.1f\"/>\n",
				endX+2, c.endY, endX+6, c.labelY, lblX-2, c.labelY)
		}
		cls := "end-lbl"
		if isMoeCurve(c) {
			cls = "end-lbl-hi"
		}
		fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"%.1f\" class=\"%s\">%s (%s)</text>\n",
			lblX, c.labelY+3.5, cls, html.EscapeString(c.shortLbl), formatMsShort(c.endMs))
	}
	for _, c := range curves {
		if !isMoeCurve(c) {
			drawCurve(c)
		}
	}
	for _, c := range curves {
		if isMoeCurve(c) {
			drawCurve(c)
		}
	}

	// Right Panel: Active OS RSS/VM + NewState cold start
	fmt.Fprintf(&sb, `
  <!-- RIGHT PANEL: Active Memory & Cold Start -->
  <text x="512" y="78" class="panel-title">Active Memory &amp; Cold Start (%d Active VMs)</text>
  <text x="512" y="94" class="panel-sub">Active %s (2,047 tables/VM) · NewState init latency · lower is better</text>

`, maxVMs, memKindTitle)

	fmt.Fprintf(&sb, "  <line x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" class=\"axis\"/>\n", rX0, plotTop, rX0, plotBottom)
	fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"393\" class=\"tick\">0</text>\n", rX0)
	for tick := memStep; tick <= memAxisMax+1e-6; tick += memStep {
		x := rX0 + (tick/memAxisMax)*rPlotW
		fmt.Fprintf(&sb, "  <line x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\" class=\"grid\"/>\n", x, plotTop, x, plotBottom)
		var tLbl string
		if tick >= 1024 {
			if math.Mod(tick, 1024) == 0 {
				tLbl = fmt.Sprintf("%.0fM", tick/1024)
			} else {
				tLbl = fmt.Sprintf("%.1fM", tick/1024)
			}
		} else {
			tLbl = fmt.Sprintf("%.0fK", tick)
		}
		fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"393\" class=\"tick\">%s</text>\n", x, tLbl)
	}

	rightRowH := (plotBottom - plotTop - 10) / float64(max(len(rightRows), 1))
	if rightRowH > 34 {
		rightRowH = 34
	}
	y := plotTop + 10
	for _, r := range rightRows {
		w := max((r.val/memAxisMax)*rPlotW, 3.0)
		lblCls := "label"
		valCls := "val"
		if r.kind == "moe" || r.kind == "moe-jit" {
			lblCls = "label-hi"
			valCls = "val-hi"
		}
		barCls := map[string]string{"moe-jit": "bar-moe-jit", "moe": "bar-moe", "ref": "bar-ref", "other": "bar-other"}[r.kind]
		fmt.Fprintf(&sb, "  <text x=\"640\" y=\"%.1f\" class=\"%s\">%s</text>\n", y+12, lblCls, html.EscapeString(r.label))
		fmt.Fprintf(&sb, "  <rect x=\"%.1f\" y=\"%.1f\" width=\"%.1f\" height=\"%.1f\" rx=\"3\" class=\"%s\"/>\n", rX0, y, w, barH, barCls)
		estTextW := float64(len([]rune(r.note))) * 7.0
		if rX0+w+6+estTextW > svgW-14 && w > estTextW+12 {
			fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"%.1f\" class=\"val\" style=\"fill:#ffffff;text-anchor:end\">%s</text>\n", rX0+w-6, y+12, html.EscapeString(r.note))
		} else {
			fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"%.1f\" class=\"%s\">%s</text>\n", rX0+w+6, y+12, valCls, html.EscapeString(r.note))
		}
		y += rightRowH
	}

	leftFoot := "Left: Go-heap Lua VMs (including Apogee JIT) scale O(VMs × objects) in Go GC; MoeLua stays near-flat via noscan Arenas (ptrdata == 0)."
	rightFoot := fmt.Sprintf("Right: isolated subprocess per engine (%d-VM idle: MoeLua %.0f KiB / %.2f ms GC", idleVMs, moeIdleKiB, moeIdleGC)
	if apogeeIdleKiB > 0 {
		rightFoot += fmt.Sprintf(" vs Apogee JIT %.0f KiB / %.1f ms GC", apogeeIdleKiB, apogeeIdleGC)
	}
	rightFoot += "). Dashed line = off-heap mmap."

	fmt.Fprintf(&sb, `
  <!-- Footer notes -->
  <text x="24" y="424" class="foot">%s</text>
  <text x="24" y="440" class="foot">%s</text>
</svg>
`, html.EscapeString(leftFoot), html.EscapeString(rightFoot))

	return sb.String(), nil
}

func writeFileMkdir(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func main() {
	benchPath := flag.String("bench", "", "path to benchreport.go -json output")
	memPath := flag.String("mem", "", "path to memcmp.go -json output")
	subtitle := flag.String("subtitle", "GOMAXPROCS=1 · Lower is better in both panels", "chart subtitle (machine, Go version)")
	dateStr := flag.String("date", time.Now().UTC().Format("2006-01-02"), "generation date shown in top-right badge (YYYY-MM-DD)")
	commitSHA := flag.String("commit", "", "optional short commit SHA appended to date badge")
	outPath := flag.String("out", filepath.Join("..", "..", "docs", "assets", "bench-compare.svg"), "output SVG path for main benchmark comparison")
	outMultiVM := flag.String("out-multivm", "", "optional output SVG path for high-density multi-VM scalability chart")
	flag.Parse()

	if *memPath == "" {
		fmt.Fprintln(os.Stderr, "genchart: -mem JSON path is required")
		os.Exit(2)
	}
	if *outPath != "" && *benchPath == "" {
		fmt.Fprintln(os.Stderr, "genchart: -bench JSON path is required when -out is set")
		os.Exit(2)
	}

	var mems []memResult
	rawMem, err := os.ReadFile(*memPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genchart: read mem json: %v\n", err)
		os.Exit(1)
	}
	if err := json.Unmarshal(rawMem, &mems); err != nil {
		fmt.Fprintf(os.Stderr, "genchart: parse mem json: %v\n", err)
		os.Exit(1)
	}
	if len(mems) == 0 {
		fmt.Fprintln(os.Stderr, "genchart: mem json is empty")
		os.Exit(1)
	}

	badge := ""
	if strings.TrimSpace(*dateStr) != "" {
		badge = "Updated " + strings.TrimSpace(*dateStr)
		if sha := strings.TrimSpace(*commitSHA); sha != "" {
			if len(sha) > 7 {
				sha = sha[:7]
			}
			badge += " · " + sha
		}
	}

	if *outPath != "" {
		var bench reportSummary
		rawBench, err := os.ReadFile(*benchPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "genchart: read bench json: %v\n", err)
			os.Exit(1)
		}
		if err := json.Unmarshal(rawBench, &bench); err != nil {
			fmt.Fprintf(os.Stderr, "genchart: parse bench json: %v\n", err)
			os.Exit(1)
		}
		svg, err := renderCompareSVG(bench, mems, *subtitle, badge)
		if err != nil {
			fmt.Fprintf(os.Stderr, "genchart: %v\n", err)
			os.Exit(1)
		}
		if err := writeFileMkdir(*outPath, svg); err != nil {
			fmt.Fprintf(os.Stderr, "genchart: write %s: %v\n", *outPath, err)
			os.Exit(1)
		}
	}

	if *outMultiVM != "" {
		svg, err := renderMultiVMSVG(mems, *subtitle, badge)
		if err != nil {
			fmt.Fprintf(os.Stderr, "genchart: %v\n", err)
			os.Exit(1)
		}
		if err := writeFileMkdir(*outMultiVM, svg); err != nil {
			fmt.Fprintf(os.Stderr, "genchart: write %s: %v\n", *outMultiVM, err)
			os.Exit(1)
		}
	}
}
