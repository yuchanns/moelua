// Copyright 2026 MoeLua Contributors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build ignore

// Benchreport copies the output of 'go test -bench' in this module to stdout
// and appends, for each suite, a table of time/op relative to C Lua, the
// median of the rounds of -count, and the geomeans. It ends with the best and
// median ms/op of every benchmark. It fails if a benchmark did.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const refEngine = "C"

var (
	// The engines in the order of the columns. refEngine ("C") is always the ratio baseline.
	engines = []string{"C", "C-O3", "MoeLua-JIT", "MoeLua", "Apogee-JIT", "Apogee-Interp", "CCGO", "ArnodelGolua", "GopherLua", "GoLua", "DCLua", "LuaJIT-joff", "LuaJIT"}
	// The column titles of the engines whose benchmark names say less.
	titles = map[string]string{"GoLua": "go-lua", "ArnodelGolua": "golua", "Apogee-Interp": "Apogee-noJIT", "MoeLua": "MoeLua-noJIT"}
	suites = []struct{ name, title string }{
		{"awfy", "are-we-fast-yet"},
		{"game", "Benchmarks Game"},
		{"scimark", "SciMark 2.0a, small"},
		{"own", "the Go engines' own benchmarks"},
		{"micro", "micro benchmarks"},
	}
	// Benchmark/CCGO/awfy/Bounce-4  1234  567890 ns/op
	reLine = regexp.MustCompile(`^Benchmark/([^/\s]+)/([^/\s]+)/([^\s]+?)(-\d+)?\s+\d+\s+([0-9.]+) ns/op`)
	// --- SKIP: Benchmark/DCLua/game/binarytrees: the reason
	reSkip = regexp.MustCompile(`^--- SKIP: Benchmark/([^/\s]+)/([^/\s]+)/([^\s:]+): (.*)`)
	// skipped are the reasons of the skipped benchmarks, keyed by
	// "engine suite/program".
	skipped = map[string]string{}
)

func title(e string) string {
	if t := titles[e]; t != "" {
		return t
	}

	return e
}

type reportSummary struct {
	Baseline      string                        `json:"baseline"`
	Rounds        int                           `json:"rounds"`
	CommonCount   int                           `json:"common_count"`
	Engines       []string                      `json:"engines"`
	CommonGeomean map[string]float64            `json:"common_geomean"`
	AllGeomean    map[string]float64            `json:"all_geomean"`
	SuiteGeomean  map[string]map[string]float64 `json:"suite_geomean"`
}

func main() {
	jsonOut := flag.String("json", "", "optional path to write structured JSON summary")
	flag.Parse()

	b := bytes.NewBuffer(nil)
	io.Copy(io.MultiWriter(os.Stdout, b), os.Stdin)
	m := map[string]map[string]map[string][]float64{} // suite: program: engine: ns/op of each round
	failed := false
	for _, v := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(v, "FAIL") || strings.HasPrefix(v, "--- FAIL") {
			failed = true
		}
		if a := reSkip.FindStringSubmatch(v); a != nil {
			skipped[a[1]+" "+a[2]+"/"+a[3]] = a[4]
		}

		a := reLine.FindStringSubmatch(v)
		if a == nil {
			continue
		}

		t, err := strconv.ParseFloat(a[5], 64)
		if err != nil {
			continue
		}

		engine, suite, prog := a[1], a[2], a[3]
		if m[suite] == nil {
			m[suite] = map[string]map[string][]float64{}
		}
		if m[suite][prog] == nil {
			m[suite][prog] = map[string][]float64{}
		}
		m[suite][prog][engine] = append(m[suite][prog][engine], t)
	}
	present := map[string]bool{}
	for _, progs := range m {
		for _, engs := range progs {
			for e := range engs {
				present[e] = true
			}
		}
	}
	engines = slices.DeleteFunc(engines, func(e string) bool { return !present[e] })
	if len(engines) == 0 {
		fmt.Fprintln(os.Stderr, "benchreport: no benchmark lines found")
		os.Exit(1)
	}
	all := map[string][]float64{}
	notMicro := map[string][]float64{}
	summary := reportSummary{
		Baseline:      refEngine,
		Engines:       slices.Clone(engines),
		CommonGeomean: map[string]float64{},
		AllGeomean:    map[string]float64{},
		SuiteGeomean:  map[string]map[string]float64{},
	}
	if present[refEngine] {
		for _, s := range suites {
			if m[s.name] != nil {
				if r := rounds(m[s.name]); r > summary.Rounds {
					summary.Rounds = r
				}
				acc := []map[string][]float64{all}
				if s.name != "micro" {
					acc = append(acc, notMicro)
				}
				suiteNums := report(s.name, s.title, m[s.name], acc...)
				summary.SuiteGeomean[s.name] = map[string]float64{}
				for _, e := range engines {
					if len(suiteNums[e]) > 0 {
						summary.SuiteGeomean[s.name][e] = geoMean(suiteNums[e])
					}
				}
			}
		}
		fmt.Printf("\n%-24s", "all but micro, geomean")
		for _, e := range engines {
			fmt.Printf("%12s", fmtRatio(notMicro[e]))
		}
		common, n := commonRatios(m)
		summary.CommonCount = n
		fmt.Printf("\n%-24s", "common, geomean")
		for _, e := range engines {
			fmt.Printf("%12s", fmtRatio(common[e]))
			if len(common[e]) > 0 {
				summary.CommonGeomean[e] = geoMean(common[e])
			}
		}
		fmt.Printf("\ncommon: the %d programs, all but micro, that every engine ran", n)
		fmt.Printf("\n%-24s", "all, geomean")
		for _, e := range engines {
			fmt.Printf("%12s", fmtRatio(all[e]))
			if len(all[e]) > 0 {
				summary.AllGeomean[e] = geoMean(all[e])
			}
		}
		fmt.Println()
	} else {
		fmt.Printf("\n(skipping C-relative ratio tables: no %q baseline samples in input; showing raw ms/op below)\n", refEngine)
	}
	times(m)
	if failed {
		fmt.Fprintln(os.Stderr, "benchreport: a benchmark failed")
		os.Exit(1)
	}
	if *jsonOut != "" {
		raw, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "benchreport: marshal json: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(*jsonOut, append(raw, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "benchreport: write json %s: %v\n", *jsonOut, err)
			os.Exit(1)
		}
	}
}

func median(s []float64) float64 {
	s = slices.Clone(s)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}

	return (s[n/2-1] + s[n/2]) / 2
}

func fmtRatio(s []float64) string {
	if len(s) == 0 {
		return "n/a"
	}

	return fmt.Sprintf("%.3f", geoMean(s))
}

// report prints the table of suite, the median times of each engine keyed by
// program, relative to C, and adds them to each of acc. A program an engine
// did not run is n/a and left out of its geomean, which is marked.
func report(suite, name string, m map[string]map[string][]float64, acc ...map[string][]float64) map[string][]float64 {
	var progs []string
	for k := range m {
		progs = append(progs, k)
	}
	slices.Sort(progs)
	fmt.Printf("\n%s, time/op relative to C Lua 5.5.1, median of %d rounds\n%-24s", name, rounds(m), "")
	for _, e := range engines {
		fmt.Printf("%12s", title(e))
	}
	hr := strings.Repeat("-", 24+12*len(engines))
	fmt.Printf("\n%s\n", hr)
	nums := map[string][]float64{}
	for _, p := range progs {
		fmt.Printf("%-24s", p)
		ref, ok := m[p][refEngine]
		for _, e := range engines {
			t, ok2 := m[p][e]
			if !ok || !ok2 {
				fmt.Printf("%12s", "n/a")
				continue
			}

			k := median(t) / median(ref)
			nums[e] = append(nums[e], k)
			for _, a := range acc {
				a[e] = append(a[e], k)
			}
			fmt.Printf("%12.3f", k)
		}
		fmt.Println()
	}
	fmt.Printf("%s\n%-24s", hr, "geomean")
	var notes []string
	for _, e := range engines {
		r := fmtRatio(nums[e])
		if n := len(nums[e]); n != 0 && n < len(progs) {
			r += "*"
			for _, p := range progs {
				if why := skipped[e+" "+suite+"/"+p]; why != "" {
					notes = append(notes, fmt.Sprintf("%s %s: %s", title(e), p, why))
				}
			}
		}
		fmt.Printf("%12s", r)
	}
	fmt.Println()
	if len(notes) != 0 {
		fmt.Printf("* over the programs the engine ran, not run: %s\n", strings.Join(notes, "; "))
	}
	return nums
}

// commonRatios returns the median times relative to C of the programs, all
// but micro, that every engine ran, and their number.
func commonRatios(m map[string]map[string]map[string][]float64) (r map[string][]float64, n int) {
	r = map[string][]float64{}
	for _, s := range suites {
		if s.name == "micro" {
			continue
		}

		for _, p := range m[s.name] {
			ref, hasRef := p[refEngine]
			if !hasRef || slices.ContainsFunc(engines, func(e string) bool { return len(p[e]) == 0 }) {
				continue
			}

			n++
			refMed := median(ref)
			for _, e := range engines {
				r[e] = append(r[e], median(p[e])/refMed)
			}
		}
	}
	return r, n
}

func rounds(m map[string]map[string][]float64) (n int) {
	for _, v := range m {
		for _, t := range v {
			n = max(n, len(t))
		}
	}
	return n
}

// times prints the best and median ms/op of every benchmark.
func times(m map[string]map[string]map[string][]float64) {
	fmt.Printf("\nms/op, best/median\n%-24s", "")
	for _, e := range engines {
		fmt.Printf("%18s", title(e))
	}
	fmt.Println()
	for _, s := range suites {
		var progs []string
		for k := range m[s.name] {
			progs = append(progs, k)
		}
		slices.Sort(progs)
		for _, p := range progs {
			fmt.Printf("%-24s", s.name+"/"+p)
			for _, e := range engines {
				t, ok := m[s.name][p][e]
				if !ok {
					fmt.Printf("%18s", "n/a")
					continue
				}

				fmt.Printf("%18s", fmt.Sprintf("%.3g/%.3g", slices.Min(t)/1e6, median(t)/1e6))
			}
			fmt.Println()
		}
	}
}

func geoMean(s []float64) float64 {
	logSum := 0.0
	for _, num := range s {
		logSum += math.Log(num)
	}

	return math.Exp(logSum / float64(len(s)))
}
