//go:build ignore

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
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

func runOne(h vmHarness, n int, asJSON bool) {
	runtime.GOMAXPROCS(1)

	// Warm up one-time package globals.
	w, wc := h.newIdle()
	wc()
	runtime.KeepAlive(w)
	w = nil
	runtime.GC()
	debug.FreeOSMemory()

	var m0, m1, m2 runtime.MemStats
	runtime.ReadMemStats(&m0)
	rss0, hasRSS := currentRSSKiB()

	vms := make([]any, n)
	closes := make([]func(), n)
	for i := range n {
		vms[i], closes[i] = h.newIdle()
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)
	rss1, _ := currentRSSKiB()
	idleHeap := float64(int64(m1.HeapAlloc)-int64(m0.HeapAlloc)) / float64(n) / 1024.0
	idleRSS := float64(rss1-rss0) / float64(n)
	idleGC := measureGC(7)

	for i := range n {
		if err := h.runInit(vms[i], activeLua); err != nil {
			panic(fmt.Sprintf("%s active init failed: %v", h.name, err))
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&m2)
	rss2, _ := currentRSSKiB()
	activeHeap := float64(int64(m2.HeapAlloc)-int64(m0.HeapAlloc)) / float64(n) / 1024.0
	activeRSS := float64(rss2-rss0) / float64(n)
	activeGC := measureGC(7)

	res := memResult{
		Engine:        h.name,
		VMs:           n,
		IdleHeapKiB:   idleHeap,
		IdleRSSKiB:    idleRSS,
		HasRSS:        hasRSS,
		IdleGCMs:      float64(idleGC) / 1e6,
		ActiveHeapKiB: activeHeap,
		ActiveRSSKiB:  activeRSS,
		ActiveGCMs:    float64(activeGC) / 1e6,
	}
	if asJSON {
		raw, err := json.Marshal(res)
		if err != nil {
			panic(err)
		}
		fmt.Println(string(raw))
	} else {
		fmt.Printf("%-15s | %11s | %11s | %8.2f ms | %12s | %12s | %9.2f ms\n",
			res.Engine,
			formatKiB(res.IdleHeapKiB, true),
			formatKiB(res.IdleRSSKiB, res.HasRSS),
			res.IdleGCMs,
			formatKiB(res.ActiveHeapKiB, true),
			formatKiB(res.ActiveRSSKiB, res.HasRSS),
			res.ActiveGCMs)
	}

	for i := range n {
		closes[i]()
		vms[i] = nil
	}
}

func main() {
	worker := flag.String("worker", "", "run single engine")
	workerJSON := flag.Bool("worker-json", false, "emit single JSON line from worker")
	jsonOut := flag.String("json", "", "optional path to write structured JSON results")
	count := flag.Int("n", 256, "number of live VMs")
	flag.Parse()

	hs := allHarnesses()
	if *worker != "" {
		for _, h := range hs {
			if h.name == *worker {
				runOne(h, *count, *workerJSON)
				return
			}
		}
		os.Exit(2)
	}

	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	fmt.Printf("%-15s | %11s | %11s | %11s | %12s | %12s | %12s\n",
		"Engine", "Idle Heap", "Idle RSS", fmt.Sprintf("%d-Idle GC", *count), "Act Heap/VM", "Act RSS/VM", fmt.Sprintf("%d-Act GC", *count))
	fmt.Println(strings.Repeat("-", 98))
	var results []memResult
	failed := false
	for _, h := range hs {
		cmd := exec.Command(self, "-worker", h.name, "-worker-json", "-n", strconv.Itoa(*count))
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s failed: %v\n", h.name, err)
			failed = true
			continue
		}
		var r memResult
		if err := json.Unmarshal(out, &r); err != nil {
			fmt.Fprintf(os.Stderr, "%s parse output failed: %v\n", h.name, err)
			failed = true
			continue
		}
		results = append(results, r)
		fmt.Printf("%-15s | %11s | %11s | %8.2f ms | %12s | %12s | %9.2f ms\n",
			r.Engine,
			formatKiB(r.IdleHeapKiB, true),
			formatKiB(r.IdleRSSKiB, r.HasRSS),
			r.IdleGCMs,
			formatKiB(r.ActiveHeapKiB, true),
			formatKiB(r.ActiveRSSKiB, r.HasRSS),
			r.ActiveGCMs)
	}
	if failed || len(results) == 0 {
		os.Exit(1)
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
