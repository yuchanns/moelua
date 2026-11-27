//go:build ignore

// genchart renders the dual-panel SVG performance chart (docs/assets/bench-compare.svg)
// from benchreport.go -json and memcmp.go -json outputs.
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

type memResult struct {
	Engine        string  `json:"engine"`
	VMs           int     `json:"vms"`
	IdleHeapKiB   float64 `json:"idle_heap_kib"`
	IdleRSSKiB    float64 `json:"idle_rss_kib"`
	HasRSS        bool    `json:"has_rss"`
	IdleGCMs      float64 `json:"idle_gc_ms"`
	ActiveHeapKiB float64 `json:"active_heap_kib"`
	ActiveRSSKiB  float64 `json:"active_rss_kib"`
	ActiveGCMs    float64 `json:"active_gc_ms"`
}

type leftRow struct {
	label string
	val   float64
	kind  string // "ref", "jit", "moe", "ccgo", "other"
}

type rightRow struct {
	label string
	ms    float64
	note  string
	kind  string // "jit", "moe", "ccgo", "other"
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

func main() {
	benchPath := flag.String("bench", "", "path to benchreport.go -json output")
	memPath := flag.String("mem", "", "path to memcmp.go -json output")
	subtitle := flag.String("subtitle", "GOMAXPROCS=1 · Lower is better in both panels", "chart subtitle (machine, Go version, date)")
	outPath := flag.String("out", filepath.Join("..", "..", "docs", "assets", "bench-compare.svg"), "output SVG path")
	flag.Parse()

	if *benchPath == "" || *memPath == "" {
		fmt.Fprintln(os.Stderr, "genchart: -bench and -mem JSON paths are required")
		os.Exit(2)
	}

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
	if _, ok := bench.CommonGeomean["C"]; !ok || bench.CommonCount == 0 {
		fmt.Fprintln(os.Stderr, "genchart: bench json missing C baseline common_geomean")
		os.Exit(1)
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

	// Build left panel rows: reference C rows first, then all Go engines (including JIT) sorted fastest-to-slowest.
	leftNames := map[string]struct {
		label string
		kind  string
	}{
		"Apogee-JIT":    {"Apogee v1.2.1 (JIT)", "jit"},
		"MoeLua":        {"MoeLua 5.5 (Pure Go)", "moe"},
		"Apogee-Interp": {"Apogee v1.2.1 (noJIT)", "other"},
		"CCGO":          {"modernc/lua (CCGO)", "ccgo"},
		"ArnodelGolua":  {"arnodel/golua (PGO)", "other"},
		"GopherLua":     {"yuin/gopher-lua", "other"},
		"GoLua":         {"Shopify/go-lua", "other"},
		"DCLua":         {"DCLua (milochristiansen)", "other"},
	}

	var leftRows []leftRow
	if v, ok := bench.CommonGeomean["C-O3"]; ok {
		leftRows = append(leftRows, leftRow{label: "C Lua 5.5.1 -O3 (ref)", val: v, kind: "ref"})
	}
	leftRows = append(leftRows, leftRow{label: "C Lua 5.5.1 -O2 (ref)", val: bench.CommonGeomean["C"], kind: "ref"})

	var goRows []leftRow
	for key, meta := range leftNames {
		if v, ok := bench.CommonGeomean[key]; ok {
			goRows = append(goRows, leftRow{label: meta.label, val: v, kind: meta.kind})
		}
	}
	slices.SortFunc(goRows, func(a, b leftRow) int {
		if c := cmp.Compare(a.val, b.val); c != 0 {
			return c
		}
		return cmp.Compare(a.label, b.label)
	})
	leftRows = append(leftRows, goRows...)

	// Build right panel rows: all Go engines (including Apogee-JIT) sorted by GC pause.
	rightLabels := map[string]struct {
		label string
		kind  string
	}{
		"MoeLua":         {"MoeLua 5.5 (Arena)", "moe"},
		"CCGO":           {"modernc/lua (mmap)", "ccgo"},
		"arnodel/golua":  {"arnodel/golua", "other"},
		"Apogee-JIT":     {"Apogee v1.2.1 (JIT)", "jit"},
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
			ms:    r.ActiveGCMs,
			note:  fmt.Sprintf("%s (%s)", formatMs(r.ActiveGCMs), formatMemSize(memVal)),
			kind:  meta.kind,
		})
	}
	if len(rightRows) == 0 {
		fmt.Fprintln(os.Stderr, "genchart: no recognized engines in mem json")
		os.Exit(1)
	}
	slices.SortFunc(rightRows, func(a, b rightRow) int {
		if c := cmp.Compare(a.ms, b.ms); c != 0 {
			return c
		}
		return cmp.Compare(a.label, b.label)
	})
	// Keep MoeLua as the top row when both MoeLua and CCGO are sub-millisecond so readers spot it immediately.
	if len(rightRows) >= 2 && rightRows[0].kind == "ccgo" && rightRows[1].kind == "moe" && rightRows[1].ms < 2.0 {
		rightRows[0], rightRows[1] = rightRows[1], rightRows[0]
	}

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
		if r.ms > maxRightMs {
			maxRightMs = r.ms
		}
	}
	rightStep := 50.0
	if maxRightMs <= 60 {
		rightStep = 20.0
	} else if maxRightMs > 250 {
		rightStep = 100.0
	}
	rightAxisMax := niceAxisMax(maxRightMs, rightStep)

	// Layout geometry
	const (
		svgW       = 960.0
		svgH       = 450.0
		leftX0     = 165.0
		leftPlotW  = 260.0
		rightX0    = 648.0
		rightPlotW = 210.0
		plotTop    = 108.0
		plotBottom = 378.0
		barH       = 17.0
	)

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" width="960" height="450" viewBox="0 0 960 450" role="img" aria-label="MoeLua 5.5 cross-engine benchmark: %d-program execution time relative to C Lua 5.5.1 -O2 and %d-VM Go GC pause">
  <defs>
    <pattern id="hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
      <rect width="6" height="6" fill="#e2e8f0"/>
      <line x1="0" y1="0" x2="0" y2="6" stroke="#94a3b8" stroke-width="2"/>
    </pattern>
    <style>
      .bg { fill: #ffffff; }
      .border { fill: none; stroke: #e2e8f0; stroke-width: 1; }
      .title { font: 700 16px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #0f172a; }
      .subtitle { font: 400 11.5px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #475569; }
      .panel-title { font: 700 13px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #0f172a; }
      .panel-sub { font: 400 11px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #64748b; }
      .label { font: 500 12px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #334155; text-anchor: end; }
      .label-hi { font: 700 12px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #047857; text-anchor: end; }
      .val { font: 600 11.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: #334155; }
      .val-hi { font: 700 12px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: #047857; }
      .grid { stroke: #f1f5f9; stroke-width: 1; }
      .axis { stroke: #cbd5e1; stroke-width: 1; }
      .refline { stroke: #64748b; stroke-width: 1; stroke-dasharray: 3 3; }
      .tick { font: 400 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: #64748b; text-anchor: middle; }
      .foot { font: 400 10.5px -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; fill: #64748b; }
      .bar-moe { fill: #10b981; }
      .bar-jit { fill: #8b5cf6; }
      .bar-other { fill: #64748b; }
      .bar-ccgo { fill: #3b82f6; }
      .bar-ref { fill: url(#hatch); stroke: #94a3b8; stroke-width: 1; }
    </style>
  </defs>

  <rect x="0.5" y="0.5" width="959" height="449" rx="10" class="bg"/>
  <rect x="0.5" y="0.5" width="959" height="449" rx="10" class="border"/>

  <!-- Header -->
  <text x="24" y="30" class="title">MoeLua 5.5 vs Go Lua Engines &amp; C Lua 5.5.1</text>
  <text x="24" y="48" class="subtitle">%s</text>

  <!-- Divider -->
  <line x1="490" y1="68" x2="490" y2="404" stroke="#e2e8f0" stroke-width="1"/>

  <!-- LEFT PANEL -->
  <text x="24" y="78" class="panel-title">Execution Time (%d-Program Geomean)</text>
  <text x="24" y="94" class="panel-sub">Relative to C Lua 5.5.1 -O2 (1.00x) · AWFY + Benchmarks Game + SciMark + Own</text>

`, bench.CommonCount, vmCount, html.EscapeString(*subtitle), bench.CommonCount)

	// Left gridlines & ticks
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
		if r.kind == "moe" {
			lblCls = "label-hi"
			valCls = "val-hi"
		}
		barCls := map[string]string{"moe": "bar-moe", "jit": "bar-jit", "ref": "bar-ref", "ccgo": "bar-ccgo", "other": "bar-other"}[r.kind]
		fmt.Fprintf(&sb, "  <text x=\"157\" y=\"%.1f\" class=\"%s\">%s</text>\n", y+13, lblCls, html.EscapeString(r.label))
		fmt.Fprintf(&sb, "  <rect x=\"%.1f\" y=\"%.1f\" width=\"%.1f\" height=\"%.1f\" rx=\"3\" class=\"%s\"/>\n", leftX0, y, w, barH, barCls)
		valX := leftX0 + w + 6
		if leftX0+w < refX+2 && valX < refX+5 {
			valX = refX + 5
		}
		fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"%.1f\" class=\"%s\">%.2fx</text>\n", valX, y+13, valCls, r.val)
		y += leftRowH
	}

	// Right panel
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
		w := max((r.ms/rightAxisMax)*rightPlotW, 3.0)
		lblCls := "label"
		valCls := "val"
		if r.kind == "moe" {
			lblCls = "label-hi"
			valCls = "val-hi"
		}
		barCls := map[string]string{"moe": "bar-moe", "jit": "bar-jit", "ccgo": "bar-ccgo", "other": "bar-other"}[r.kind]
		fmt.Fprintf(&sb, "  <text x=\"640\" y=\"%.1f\" class=\"%s\">%s</text>\n", y+13, lblCls, html.EscapeString(r.label))
		fmt.Fprintf(&sb, "  <rect x=\"%.1f\" y=\"%.1f\" width=\"%.1f\" height=\"%.1f\" rx=\"3\" class=\"%s\"/>\n", rightX0, y, w, barH, barCls)
		// Place note inside bar if it would cross the right card border (~945px).
		estTextW := float64(len(r.note)) * 7.0
		if rightX0+w+6+estTextW > svgW-14 && w > estTextW+12 {
			fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"%.1f\" class=\"val\" style=\"fill:#ffffff;text-anchor:end\">%s</text>\n", rightX0+w-6, y+13, html.EscapeString(r.note))
		} else {
			fmt.Fprintf(&sb, "  <text x=\"%.1f\" y=\"%.1f\" class=\"%s\">%s</text>\n", rightX0+w+6, y+13, valCls, html.EscapeString(r.note))
		}
		y += rightRowH
	}

	// Footer notes
	leftFoot := fmt.Sprintf("Left: median of %d rounds via bench/compare (32/32 byte-exact output vs C Lua 5.5.1). Purple = native JIT; green = MoeLua (pure-Go interpreter).", max(bench.Rounds, 1))
	if v, ok := bench.CommonGeomean["LuaJIT"]; ok {
		leftFoot += fmt.Sprintf(" LuaJIT: %.2fx.", v)
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

	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "genchart: mkdir: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*outPath, []byte(sb.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "genchart: write %s: %v\n", *outPath, err)
		os.Exit(1)
	}
}
