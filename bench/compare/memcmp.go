//go:build ignore

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	golua "github.com/Shopify/go-lua"
	arnodel "github.com/arnodel/golua/lib"
	arnodelrt "github.com/arnodel/golua/runtime"
	apogeelua "github.com/matjam/apogee/lua"
	apogeestdlib "github.com/matjam/apogee/stdlib"
	dclua "github.com/milochristiansen/lua"
	"github.com/milochristiansen/lua/lmodbase"
	"github.com/milochristiansen/lua/lmodmath"
	"github.com/milochristiansen/lua/lmodpackage"
	"github.com/milochristiansen/lua/lmodstring"
	"github.com/milochristiansen/lua/lmodtable"
	"github.com/milochristiansen/lua/lmodutf8"
	"github.com/yuchanns/moelua"
	gopher "github.com/yuin/gopher-lua"
	"modernc.org/lua"
)

const activeLua = `
local function makeTree(d)
  if d == 0 then return { x = 1, y = 2 } end
  return { l = makeTree(d - 1), r = makeTree(d - 1), tag = d }
end
G_TREE = makeTree(10) -- 2047 tables per VM
`

func medianDur(ds []time.Duration) time.Duration {
	c := slices.Clone(ds)
	slices.Sort(c)
	return c[len(c)/2]
}

func measureGC(rounds int) time.Duration {
	ds := make([]time.Duration, rounds)
	for i := range rounds {
		t0 := time.Now()
		runtime.GC()
		ds[i] = time.Since(t0)
	}
	return medianDur(ds)
}

func currentRSSKiB() (int64, bool) {
	if runtime.GOOS == "windows" {
		return 0, false
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

type vmHarness struct {
	name    string
	newIdle func() (keep any, closeFn func())
	runInit func(keep any, code string) error
}

func allHarnesses() []vmHarness {
	return []vmHarness{
		{
			name: "MoeLua",
			newIdle: func() (any, func()) {
				s := moelua.New()
				return s, s.Close
			},
			runInit: func(k any, code string) error {
				_, err := k.(*moelua.State).DoString(code)
				return err
			},
		},
		{
			name: "Apogee-noJIT",
			newIdle: func() (any, func()) {
				l := apogeelua.NewState(apogeelua.WithoutJIT())
				apogeestdlib.Open(l)
				return l, l.Close
			},
			runInit: func(k any, code string) error {
				return k.(*apogeelua.State).DoString(code)
			},
		},
		{
			name: "Apogee-JIT",
			newIdle: func() (any, func()) {
				l := apogeelua.NewState()
				apogeestdlib.Open(l)
				return l, l.Close
			},
			runInit: func(k any, code string) error {
				return k.(*apogeelua.State).DoString(code)
			},
		},
		{
			name: "CCGO",
			newIdle: func() (any, func()) {
				m, err := lua.NewVM()
				if err != nil {
					panic(err)
				}
				return m, func() { _ = m.Close() }
			},
			runInit: func(k any, code string) error {
				_, err := k.(*lua.VM).Eval(code)
				return err
			},
		},
		{
			name: "arnodel/golua",
			newIdle: func() (any, func()) {
				r := arnodelrt.New(nil)
				cleanup := arnodel.LoadAll(r)
				return r, cleanup
			},
			runInit: func(k any, code string) error {
				r := k.(*arnodelrt.Runtime)
				ch, err := r.CompileAndLoadLuaChunk("init", []byte(code), arnodelrt.TableValue(r.GlobalEnv()))
				if err != nil {
					return err
				}
				_, err = arnodelrt.Call1(r.MainThread(), arnodelrt.FunctionValue(ch))
				return err
			},
		},
		{
			name: "GopherLua",
			newIdle: func() (any, func()) {
				L := gopher.NewState()
				return L, L.Close
			},
			runInit: func(k any, code string) error {
				return k.(*gopher.LState).DoString(code)
			},
		},
		{
			name: "Shopify/go-lua",
			newIdle: func() (any, func()) {
				l := golua.NewState()
				golua.OpenLibraries(l)
				return l, func() {}
			},
			runInit: func(k any, code string) error {
				return golua.DoString(k.(*golua.State), code)
			},
		},
		{
			name: "DCLua",
			newIdle: func() (any, func()) {
				l := dclua.NewState()
				_ = l.Protect(func() {
					for _, f := range []dclua.NativeFunction{lmodbase.Open, lmodpackage.Open, lmodstring.Open, lmodtable.Open, lmodmath.Open, lmodutf8.Open} {
						l.Push(f)
						l.Call(0, 0)
					}
				})
				return l, func() {}
			},
			runInit: func(k any, code string) error {
				l := k.(*dclua.State)
				if err := l.LoadText(strings.NewReader(code), "init", 0); err != nil {
					return err
				}
				return l.PCall(0, 0)
			},
		},
	}
}

func formatKiB(v float64, ok bool) string {
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%.1f KB", v)
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

func runCLuaRef(bin string, idleCount, activeCount int) (*memResult, error) {
	if _, err := os.Stat(bin); err != nil {
		return nil, nil
	}
	idleCmd := exec.Command(bin, "idle", strconv.Itoa(idleCount))
	idleCmd.Stderr = os.Stderr
	idleOut, err := idleCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("clua_mem idle failed: %w", err)
	}
	var idleRes memResult
	if err := json.Unmarshal(idleOut, &idleRes); err != nil {
		return nil, fmt.Errorf("clua_mem parse idle failed: %w", err)
	}

	actCmd := exec.Command(bin, "active", strconv.Itoa(activeCount))
	actCmd.Stderr = os.Stderr
	actOut, err := actCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("clua_mem active failed: %w", err)
	}
	var actRes memResult
	if err := json.Unmarshal(actOut, &actRes); err != nil {
		return nil, fmt.Errorf("clua_mem parse active failed: %w", err)
	}

	res := &memResult{
		Engine:       "C",
		VMs:          idleCount,
		NewStateUs:   idleRes.NewStateUs,
		IdleRSSKiB:   idleRes.IdleRSSKiB,
		HasRSS:       idleRes.HasRSS && actRes.HasRSS,
		ActiveRSSKiB: actRes.ActiveRSSKiB,
		Scale: []scalePoint{
			{VMs: activeCount, ActiveRSSKiB: actRes.ActiveRSSKiB},
		},
	}
	return res, nil
}

func parseScaleList(raw string) ([]int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid scale count %q", part)
		}
		out = append(out, n)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	return out, nil
}

func warmupHarness(h vmHarness) {
	w, wc := h.newIdle()
	wc()
	runtime.KeepAlive(w)
	w = nil
	runtime.GC()
	debug.FreeOSMemory()
}

func runIdleWorker(h vmHarness, n int) {
	runtime.GOMAXPROCS(1)
	warmupHarness(h)

	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	rss0, hasRSS := currentRSSKiB()

	vms := make([]any, n)
	closes := make([]func(), n)
	t0 := time.Now()
	for i := range n {
		vms[i], closes[i] = h.newIdle()
	}
	createDur := time.Since(t0)

	runtime.GC()
	runtime.ReadMemStats(&m1)
	rss1, _ := currentRSSKiB()
	idleHeap := float64(int64(m1.HeapAlloc)-int64(m0.HeapAlloc)) / float64(n) / 1024.0
	idleRSS := float64(rss1-rss0) / float64(n)
	idleGC := measureGC(7)

	res := memResult{
		Engine:      h.name,
		VMs:         n,
		NewStateUs:  float64(createDur.Microseconds()) / float64(n),
		IdleHeapKiB: idleHeap,
		IdleRSSKiB:  idleRSS,
		HasRSS:      hasRSS,
		IdleGCMs:    float64(idleGC) / 1e6,
	}
	raw, err := json.Marshal(res)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(raw))

	for i := range n {
		closes[i]()
		vms[i] = nil
	}
}

func runActiveScaleWorker(h vmHarness, counts []int) {
	runtime.GOMAXPROCS(1)
	warmupHarness(h)

	var m0, mCur runtime.MemStats
	runtime.ReadMemStats(&m0)
	rss0, hasRSS := currentRSSKiB()

	maxN := counts[len(counts)-1]
	vms := make([]any, 0, maxN)
	closes := make([]func(), 0, maxN)

	points := make([]scalePoint, 0, len(counts))
	for _, target := range counts {
		for len(vms) < target {
			v, c := h.newIdle()
			if err := h.runInit(v, activeLua); err != nil {
				panic(fmt.Sprintf("%s active init failed at VM %d: %v", h.name, len(vms), err))
			}
			vms = append(vms, v)
			closes = append(closes, c)
		}
		runtime.GC()
		runtime.ReadMemStats(&mCur)
		rssCur, _ := currentRSSKiB()
		gcDur := measureGC(5)
		points = append(points, scalePoint{
			VMs:           target,
			ActiveHeapKiB: float64(int64(mCur.HeapAlloc)-int64(m0.HeapAlloc)) / float64(target) / 1024.0,
			ActiveRSSKiB:  float64(rssCur-rss0) / float64(target),
			ActiveGCMs:    float64(gcDur) / 1e6,
		})
	}

	res := memResult{
		Engine: h.name,
		HasRSS: hasRSS,
		Scale:  points,
	}
	raw, err := json.Marshal(res)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(raw))

	for i := range vms {
		closes[i]()
		vms[i] = nil
	}
}

func main() {
	worker := flag.String("worker", "", "run single engine")
	workerMode := flag.String("worker-mode", "idle", "worker mode: idle or active-scale")
	jsonOut := flag.String("json", "", "optional path to write structured JSON results")
	enginesFlag := flag.String("engines", "", "optional regex filter for engines (matches Go engine names and C)")
	cluaMemBin := flag.String("clua-mem", "../../build/clua_mem", "optional path to compiled C Lua 5.5.1 memory reference helper")
	count := flag.Int("n", 256, "primary VM count for summary table")
	scaleFlag := flag.String("scale", "64,256,512,1000", "comma-separated active VM counts for scaling curve")
	flag.Parse()

	var engineRE *regexp.Regexp
	if *enginesFlag != "" {
		var err error
		engineRE, err = regexp.Compile(*enginesFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memcmp: invalid -engines regex %q: %v\n", *enginesFlag, err)
			os.Exit(2)
		}
	}

	scaleCounts, err := parseScaleList(*scaleFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memcmp: %v\n", err)
		os.Exit(2)
	}
	if !slices.Contains(scaleCounts, *count) {
		scaleCounts = append(scaleCounts, *count)
		slices.Sort(scaleCounts)
	}
	var scaleStrs []string
	for _, c := range scaleCounts {
		scaleStrs = append(scaleStrs, strconv.Itoa(c))
	}
	scaleArg := strings.Join(scaleStrs, ",")

	hs := allHarnesses()
	if *worker != "" {
		for _, h := range hs {
			if h.name == *worker {
				switch *workerMode {
				case "idle":
					runIdleWorker(h, *count)
				case "active-scale":
					runActiveScaleWorker(h, scaleCounts)
				default:
					fmt.Fprintf(os.Stderr, "memcmp: unknown worker-mode %q\n", *workerMode)
					os.Exit(2)
				}
				return
			}
		}
		os.Exit(2)
	}

	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	fmt.Printf("%-15s | %9s | %11s | %11s | %11s | %12s | %12s | %12s\n",
		"Engine", "NewState", "Idle Heap", "Idle RSS", fmt.Sprintf("%d-Idle GC", *count), "Act Heap/VM", "Act RSS/VM", fmt.Sprintf("%d-Act GC", *count))
	fmt.Println(strings.Repeat("-", 110))
	var results []memResult
	failed := false
	for _, h := range hs {
		if engineRE != nil && !engineRE.MatchString(h.name) {
			continue
		}
		idleCmd := exec.Command(self, "-worker", h.name, "-worker-mode", "idle", "-n", strconv.Itoa(*count), "-scale", scaleArg)
		idleCmd.Stderr = os.Stderr
		idleOut, err := idleCmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s idle worker failed: %v\n", h.name, err)
			failed = true
			continue
		}
		var idleRes memResult
		if err := json.Unmarshal(idleOut, &idleRes); err != nil {
			fmt.Fprintf(os.Stderr, "%s parse idle output failed: %v\n", h.name, err)
			failed = true
			continue
		}

		scaleCmd := exec.Command(self, "-worker", h.name, "-worker-mode", "active-scale", "-n", strconv.Itoa(*count), "-scale", scaleArg)
		scaleCmd.Stderr = os.Stderr
		scaleOut, err := scaleCmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s active-scale worker failed: %v\n", h.name, err)
			failed = true
			continue
		}
		var scaleRes memResult
		if err := json.Unmarshal(scaleOut, &scaleRes); err != nil {
			fmt.Fprintf(os.Stderr, "%s parse active-scale output failed: %v\n", h.name, err)
			failed = true
			continue
		}

		r := idleRes
		r.HasRSS = idleRes.HasRSS && scaleRes.HasRSS
		r.Scale = scaleRes.Scale
		for _, pt := range scaleRes.Scale {
			if pt.VMs == *count {
				r.ActiveHeapKiB = pt.ActiveHeapKiB
				r.ActiveRSSKiB = pt.ActiveRSSKiB
				r.ActiveGCMs = pt.ActiveGCMs
				break
			}
		}
		results = append(results, r)
		fmt.Printf("%-15s | %6.1f us | %11s | %11s | %8.2f ms | %12s | %12s | %9.2f ms\n",
			r.Engine,
			r.NewStateUs,
			formatKiB(r.IdleHeapKiB, true),
			formatKiB(r.IdleRSSKiB, r.HasRSS),
			r.IdleGCMs,
			formatKiB(r.ActiveHeapKiB, true),
			formatKiB(r.ActiveRSSKiB, r.HasRSS),
			r.ActiveGCMs)
	}
	if *cluaMemBin != "" && (engineRE == nil || engineRE.MatchString("C")) {
		maxScale := scaleCounts[len(scaleCounts)-1]
		cRes, err := runCLuaRef(*cluaMemBin, *count, maxScale)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		if cRes != nil {
			results = append(results, *cRes)
			fmt.Printf("%-15s | %6.1f us | %11s | %11s | %11s | %12s | %12s | %12s\n",
				"C Lua 5.5.1",
				cRes.NewStateUs,
				"n/a",
				formatKiB(cRes.IdleRSSKiB, cRes.HasRSS),
				"n/a",
				"n/a",
				formatKiB(cRes.ActiveRSSKiB, cRes.HasRSS),
				"n/a")
		}
	}
	if failed || len(results) == 0 {
		os.Exit(1)
	}

	hasGoScaling := false
	for _, r := range results {
		if r.Engine != "C" {
			hasGoScaling = true
			break
		}
	}
	if hasGoScaling {
		fmt.Printf("\nHigh-Density Active Go GC Scaling (2,047 tables/VM):\n%-15s", "Engine")
		for _, c := range scaleCounts {
			fmt.Printf(" | %10s", fmt.Sprintf("%d VMs", c))
		}
		fmt.Println()
		fmt.Println(strings.Repeat("-", 15+13*len(scaleCounts)))
		for _, r := range results {
			if r.Engine == "C" {
				continue
			}
			fmt.Printf("%-15s", r.Engine)
			for _, pt := range r.Scale {
				fmt.Printf(" | %7.2f ms", pt.ActiveGCMs)
			}
			fmt.Println()
		}
	}

	if *jsonOut != "" {
		raw, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "memcmp: marshal json: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(*jsonOut, append(raw, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "memcmp: write json %s: %v\n", *jsonOut, err)
			os.Exit(1)
		}
	}
}
