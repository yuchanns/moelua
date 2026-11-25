package main

import (
	"bytes"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"time"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/vm"
)

type timing struct {
	wall   time.Duration
	cpu    time.Duration
	maxRSS int64 // bytes
}

type stats struct {
	wallMed time.Duration
	wallIQR time.Duration
	cpuMed  time.Duration
	cpuIQR  time.Duration
	rssMax  int64 // bytes
}

func runCOnce(luaBin, scriptPath string) (string, timing, error) {
	start := time.Now()
	cmd := exec.Command(luaBin, scriptPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", timing{}, fmt.Errorf("%v: %s", err, stderr.String())
	}
	wall := time.Since(start)
	cpu := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	rss := rusageMaxRSSBytes(cmd.ProcessState)
	return strings.TrimSpace(stdout.String()), timing{wall: wall, cpu: cpu, maxRSS: rss}, nil
}

func runMoeSubprocOnce(selfBin, chunkPath string, gomax int, tier1 bool) (string, timing, error) {
	args := []string{"-worker", chunkPath}
	if gomax > 0 {
		args = append(args, fmt.Sprintf("-gomaxprocs=%d", gomax))
	}
	if tier1 {
		args = append(args, "-tier1")
	}
	start := time.Now()
	cmd := exec.Command(selfBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", timing{}, fmt.Errorf("%v: %s", err, stderr.String())
	}
	wall := time.Since(start)
	cpu := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	rss := rusageMaxRSSBytes(cmd.ProcessState)
	return strings.TrimSpace(stdout.String()), timing{wall: wall, cpu: cpu, maxRSS: rss}, nil
}

func runMoeOnce(proto *bytecode.Proto, tier1Proto *bytecode.Proto) (string, timing, error) {
	runtime.GC()
	var buf bytes.Buffer
	cpu0 := getSelfCPU()
	t0 := time.Now()
	s := vm.NewState()
	s.SetOutput(&buf)
	if tier1Proto != nil {
		t1cl, err := s.Load(tier1Proto)
		if err != nil {
			return "", timing{}, err
		}
		if _, err := s.Call(vm.ClosureValue(t1cl)); err != nil {
			return "", timing{}, err
		}
	}
	cl, err := s.Load(proto)
	if err != nil {
		return "", timing{}, err
	}
	if _, err := s.Call(vm.ClosureValue(cl)); err != nil {
		return "", timing{}, err
	}
	wall := time.Since(t0)
	cpu := getSelfCPU() - cpu0
	return strings.TrimSpace(buf.String()), timing{wall: wall, cpu: cpu}, nil
}

func percentileDur(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo] + time.Duration(frac*float64(sorted[hi]-sorted[lo]))
}

func computeStats(ts []timing) stats {
	walls := make([]time.Duration, len(ts))
	cpus := make([]time.Duration, len(ts))
	var maxRSS int64
	for i, t := range ts {
		walls[i] = t.wall
		cpus[i] = t.cpu
		if t.maxRSS > maxRSS {
			maxRSS = t.maxRSS
		}
	}
	slices.Sort(walls)
	slices.Sort(cpus)
	return stats{
		wallMed: percentileDur(walls, 0.50),
		wallIQR: percentileDur(walls, 0.75) - percentileDur(walls, 0.25),
		cpuMed:  percentileDur(cpus, 0.50),
		cpuIQR:  percentileDur(cpus, 0.75) - percentileDur(cpus, 0.25),
		rssMax:  maxRSS,
	}
}

func runWorker(chunkPath string, tier1 bool) {
	chunkBytes, err := os.ReadFile(chunkPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read chunk: %v\n", err)
		os.Exit(1)
	}
	proto, err := bytecode.Undump(chunkBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "undump: %v\n", err)
		os.Exit(1)
	}
	s := vm.NewState()
	defer s.Close()
	if tier1 {
		t1p, err := compiler.Compile("=tier1", `setmetatable({}, {__mode = "k"})`)
		if err != nil {
			panic(err)
		}
		t1cl, err := s.Load(t1p)
		if err != nil {
			panic(err)
		}
		if _, err := s.Call(vm.ClosureValue(t1cl)); err != nil {
			panic(err)
		}
	}
	cl, err := s.Load(proto)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}
	if _, err := s.Call(vm.ClosureValue(cl)); err != nil {
		fmt.Fprintf(os.Stderr, "exec: %v\n", err)
		os.Exit(1)
	}
}

func main() {
	rounds := flag.Int("rounds", 10, "interleaved ABAB timed rounds per benchmark (median + IQR)")
	only := flag.String("only", "", "comma-separated subset of benchmark names")
	gomax := flag.Int("gomaxprocs", 0, "override GOMAXPROCS (0 = default)")
	cpuprof := flag.String("cpuprofile", "", "write CPU profile to file (for PGO)")
	tier1 := flag.Bool("tier1", false, "pre-activate Tier 1 GC semantics via dummy weak table")
	subproc := flag.Bool("subproc", false, "run MoeLua in a fresh child process per round (equal footing with C)")
	worker := flag.String("worker", "", "internal: execute compiled .luac file and exit")
	o3BinFlag := flag.String("lua-o3", "", "override C -O3 binary path (default: build/lua55_o3)")
	flag.Parse()

	if *gomax > 0 {
		runtime.GOMAXPROCS(*gomax)
	}
	if *worker != "" {
		runWorker(*worker, *tier1)
		return
	}

	if *cpuprof != "" {
		f, err := os.Create(*cpuprof)
		if err != nil {
			panic(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			panic(err)
		}
		defer func() {
			pprof.StopCPUProfile()
			_ = f.Close()
		}()
	}

	var tier1Proto *bytecode.Proto
	if *tier1 {
		var err error
		tier1Proto, err = compiler.Compile("=tier1", `setmetatable({}, {__mode = "k"})`)
		if err != nil {
			panic(err)
		}
	}

	root, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	selfBin, err := os.Executable()
	if err != nil {
		panic(err)
	}
	luaO2 := filepath.Join(root, "build", "lua55_o2")
	luaO3 := filepath.Join(root, "build", "lua55_o3")
	if *o3BinFlag != "" {
		luaO3 = *o3BinFlag
	}
	luac := filepath.Join(root, "build", "luac55")

	names := []string{
		// Class (a): tight arithmetic / recursion
		"fib", "forloop_int", "sieve", "mandelbrot", "fannkuch",
		// Class (b): OOP / metatable / stdlib
		"richards", "deltablue", "nbody", "binarytrees", "ecs_stdlib",
		// Class (c): dense numeric arrays (sort_int excluded from core VM geomean)
		"spectralnorm", "matmul", "sort_int",
	}
	if *only != "" {
		names = strings.Split(*only, ",")
	}

	tierLabel := "tier0"
	if *tier1 {
		tierLabel = "tier1"
	}
	execMode := "inproc"
	if *subproc {
		execMode = "subproc"
	}
	fmt.Printf("MoeLua Benchmark Harness | mode=%s polarity=%s gc=%s exec=%s rounds=%d | GOMAXPROCS=%d | %s/%s | %s\n",
		vm.ModeName, vm.PolarityName, tierLabel, execMode, *rounds, runtime.GOMAXPROCS(0), runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Printf("%-13s | %15s | %15s | %15s | %7s %7s | %7s | %14s\n",
		"Benchmark", "C-O2 cpu(±IQR)", "C-O3 cpu(±IQR)", "Moe cpu(±IQR)", "vs O2 C", "vs O2 W", "vs O3 C", "RSS C/Moe MiB")
	fmt.Println(strings.Repeat("-", 116))

	tmpDir, err := os.MkdirTemp("", "moelua-bench-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmpDir)

	var vmLogSumO2C, vmLogSumO2W, vmLogSumO3C float64
	vmCount := 0

	for _, name := range names {
		script := filepath.Join(root, "bench", "scripts", name+".lua")
		chunkPath := filepath.Join(tmpDir, name+".luac")
		if out, err := exec.Command(luac, "-o", chunkPath, script).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "luac %s failed: %v\n%s\n", name, err, out)
			os.Exit(1)
		}
		chunkBytes, err := os.ReadFile(chunkPath)
		if err != nil {
			panic(err)
		}
		proto, err := bytecode.Undump(chunkBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "undump %s failed: %v\n", name, err)
			os.Exit(1)
		}

		// Warmup + verify exact output match + clean subprocess Peak RSS probe.
		wantOut, _, err := runCOnce(luaO2, script)
		if err != nil {
			fmt.Fprintf(os.Stderr, "C-O2 %s failed: %v\n", name, err)
			os.Exit(1)
		}
		gotOut, moeProbe, err := runMoeSubprocOnce(selfBin, chunkPath, *gomax, *tier1)
		if err != nil {
			fmt.Fprintf(os.Stderr, "MoeLua subproc %s failed: %v\n", name, err)
			os.Exit(1)
		}
		if gotOut != wantOut {
			fmt.Fprintf(os.Stderr, "OUTPUT MISMATCH on %s!\n  C:      %q\n  MoeLua: %q\n", name, wantOut, gotOut)
			os.Exit(1)
		}
		if !*subproc {
			gotIn, _, err := runMoeOnce(proto, tier1Proto)
			if err != nil {
				fmt.Fprintf(os.Stderr, "MoeLua inproc %s failed: %v\n", name, err)
				os.Exit(1)
			}
			if gotIn != wantOut {
				fmt.Fprintf(os.Stderr, "INPROC OUTPUT MISMATCH on %s!\n", name)
				os.Exit(1)
			}
		}

		var c2t, c3t, mt []timing
		for r := range *rounds {
			runMoe := func() (timing, error) {
				if *subproc {
					_, tm, err := runMoeSubprocOnce(selfBin, chunkPath, *gomax, *tier1)
					return tm, err
				}
				_, tm, err := runMoeOnce(proto, tier1Proto)
				return tm, err
			}
			// Interleaved ABAB ordering (alternating C-O2/Moe/C-O3 and C-O3/Moe/C-O2)
			if r%2 == 0 {
				_, t2, err := runCOnce(luaO2, script)
				if err != nil {
					panic(err)
				}
				tm, err := runMoe()
				if err != nil {
					panic(err)
				}
				_, t3, err := runCOnce(luaO3, script)
				if err != nil {
					panic(err)
				}
				c2t = append(c2t, t2)
				mt = append(mt, tm)
				c3t = append(c3t, t3)
			} else {
				_, t3, err := runCOnce(luaO3, script)
				if err != nil {
					panic(err)
				}
				tm, err := runMoe()
				if err != nil {
					panic(err)
				}
				_, t2, err := runCOnce(luaO2, script)
				if err != nil {
					panic(err)
				}
				c2t = append(c2t, t2)
				mt = append(mt, tm)
				c3t = append(c3t, t3)
			}
		}
		s2 := computeStats(c2t)
		s3 := computeStats(c3t)
		sm := computeStats(mt)
		moeRSS := sm.rssMax
		if moeRSS == 0 {
			moeRSS = moeProbe.maxRSS
		}

		ratioO2C := float64(s2.cpuMed) / float64(sm.cpuMed)
		ratioO2W := float64(s2.wallMed) / float64(sm.wallMed)
		ratioO3C := float64(s3.cpuMed) / float64(sm.cpuMed)

		if name != "sort_int" {
			vmLogSumO2C += math.Log(ratioO2C)
			vmLogSumO2W += math.Log(ratioO2W)
			vmLogSumO3C += math.Log(ratioO3C)
			vmCount++
		}

		fmt.Printf("%-13s | %6.1f (±%4.1f)ms | %6.1f (±%4.1f)ms | %6.1f (±%4.1f)ms | %6.2fx %6.2fx | %6.2fx | %5.1f / %5.1f\n",
			name,
			float64(s2.cpuMed)/1e6, float64(s2.cpuIQR)/1e6,
			float64(s3.cpuMed)/1e6, float64(s3.cpuIQR)/1e6,
			float64(sm.cpuMed)/1e6, float64(sm.cpuIQR)/1e6,
			ratioO2C, ratioO2W, ratioO3C,
			float64(s2.rssMax)/(1024*1024), float64(moeRSS)/(1024*1024))
	}
	if vmCount > 0 {
		fmt.Println(strings.Repeat("-", 116))
		fmt.Printf("%-13s | %49s | %6.2fx %6.2fx | %6.2fx | (excl. sort_int, n=%d)\n",
			"VM GEOMEAN", "",
			math.Exp(vmLogSumO2C/float64(vmCount)),
			math.Exp(vmLogSumO2W/float64(vmCount)),
			math.Exp(vmLogSumO3C/float64(vmCount)),
			vmCount)
	}
}
