package bench_test

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/yuchanns/moelua/vm"
)

// BenchmarkMultiVMGoGCScan measures Go runtime.GC() mark/pause latency across many
// simultaneously live independent Lua 5.5.1 States, each holding live Lua tables,
// nested records, dynamic strings, closures, and a suspended coroutine inside its
// per-State noscan []byte Arena.
func BenchmarkMultiVMGoGCScan(b *testing.B) {
	setupProto := compileChunk(b, `
		local items = {}
		for i = 1, 200 do
			items[i] = {
				id = i,
				name = "node_" .. i,
				meta = { active = (i % 2 == 0), weight = i * 0.5 },
				fn = function(x) return x + i end,
			}
		end
		local co = coroutine.create(function()
			local acc = 0
			for i = 1, #items do
				acc = acc + items[i].id
				if i == 100 then coroutine.yield(acc) end
			end
			return acc
		end)
		coroutine.resume(co)
		STATE = { items = items, co = co }
		return #items
	`)

	for _, numVMs := range []int{256, 1000} {
		b.Run(fmt.Sprintf("%dVMs", numVMs), func(b *testing.B) {
			runtime.GC()
			var mBefore runtime.MemStats
			runtime.ReadMemStats(&mBefore)

			states := make([]*vm.State, numVMs)
			var totalBacking int64
			var totalAlloc int64
			var totalLiveObjs int64
			for i := range numVMs {
				s := vm.NewState()
				cl, err := s.Load(setupProto)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := s.Call(vm.ClosureValue(cl)); err != nil {
					b.Fatal(err)
				}
				st := s.ArenaStats()
				totalBacking += st.BackingBytes
				totalAlloc += st.AllocBytes
				totalLiveObjs += st.LiveObjects
				states[i] = s
			}
			defer func() {
				for _, s := range states {
					s.Close()
				}
			}()

			runtime.GC()
			var mAfter runtime.MemStats
			runtime.ReadMemStats(&mAfter)

			heapDelta := int64(mAfter.HeapAlloc) - int64(mBefore.HeapAlloc)
			if heapDelta < 0 {
				heapDelta = int64(mAfter.HeapAlloc)
			}

			b.ResetTimer()
			for b.Loop() {
				runtime.GC()
			}
			b.ReportMetric(float64(heapDelta)/float64(numVMs)/1024.0, "heap-KiB/VM")
			b.ReportMetric(float64(totalBacking)/float64(numVMs)/1024.0, "arena-KiB/VM")
			b.ReportMetric(float64(totalAlloc)/float64(numVMs)/1024.0, "live-KiB/VM")
			b.ReportMetric(float64(totalLiveObjs)/float64(numVMs), "lua-objs/VM")
			runtime.KeepAlive(states)
		})
	}
}

// BenchmarkIdleVMFootprint1000 measures steady-state Go HeapAlloc and Arena BackingBytes
// across 1,000 freshly instantiated idle Lua 5.5.1 States with full standard libraries.
func BenchmarkIdleVMFootprint1000(b *testing.B) {
	const numVMs = 1000
	// Ensure template is initialized outside measurement delta.
	warm := vm.NewState()
	warm.Close()

	runtime.GC()
	var mBefore runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	states := make([]*vm.State, numVMs)
	var totalBacking int64
	var totalAlloc int64
	for i := range numVMs {
		s := vm.NewState()
		st := s.ArenaStats()
		totalBacking += st.BackingBytes
		totalAlloc += st.AllocBytes
		states[i] = s
	}
	defer func() {
		for _, s := range states {
			s.Close()
		}
	}()

	runtime.GC()
	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)

	heapDelta := int64(mAfter.HeapAlloc) - int64(mBefore.HeapAlloc)
	if heapDelta < 0 {
		heapDelta = int64(mAfter.HeapAlloc)
	}

	b.ResetTimer()
	for b.Loop() {
		runtime.GC()
	}
	b.ReportMetric(float64(heapDelta)/float64(numVMs)/1024.0, "heap-KiB/VM")
	b.ReportMetric(float64(totalBacking)/float64(numVMs)/1024.0, "arena-KiB/VM")
	b.ReportMetric(float64(totalAlloc)/float64(numVMs)/1024.0, "live-KiB/VM")
	runtime.KeepAlive(states)
}

// BenchmarkMultiVMServiceFootprint1000 models 1,000 isolated long-lived service VMs
// that each compile/load a Lua script with handlers, tables, and 2 suspended coroutines,
// perform transient startup allocations, and run collectgarbage("collect") to settle.
func BenchmarkMultiVMServiceFootprint1000(b *testing.B) {
	const numVMs = 1000
	serviceSrc := `
		local handlers = {}
		local cfg = { name = "svc", version = 1, retries = 3, timeout_ms = 5000 }
		local function dispatch(cmd, a, b)
			local fn = handlers[cmd]
			if not fn then return nil, "unknown" end
			return fn(a, b)
		end
		handlers.ping = function(x) return "pong:" .. tostring(x) end
		handlers.add = function(a, b) return (a or 0) + (b or 0) end
		handlers.info = function() return cfg.name, cfg.version end

		-- Simulate startup churn (temporary tables & strings) before settling.
		local tmp = {}
		for i = 1, 200 do
			tmp[i] = { k = "boot_" .. i, v = i * 2 }
		end
		tmp = nil

		local worker1 = coroutine.create(function(init)
			local n = init or 0
			while true do
				local delta = coroutine.yield(n)
				n = n + (delta or 1)
			end
		end)
		local worker2 = coroutine.create(function()
			local log = {}
			while true do
				local ev = coroutine.yield(#log)
				log[#log + 1] = ev
			end
		end)
		coroutine.resume(worker1, 10)
		coroutine.resume(worker2)
		SERVICE = { cfg = cfg, dispatch = dispatch, w1 = worker1, w2 = worker2 }
		collectgarbage("collect")
		return dispatch("ping", 1)
	`

	warm := vm.NewState()
	warm.Close()

	runtime.GC()
	var mBefore runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	states := make([]*vm.State, numVMs)
	var totalBacking int64
	var totalAlloc int64
	var totalLiveObjs int64
	for i := range numVMs {
		s := vm.NewState()
		cl, err := s.LoadChunk([]byte(serviceSrc), "=service.lua")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := s.Call(cl); err != nil {
			b.Fatal(err)
		}
		st := s.ArenaStats()
		totalBacking += st.BackingBytes
		totalAlloc += st.AllocBytes
		totalLiveObjs += st.LiveObjects
		states[i] = s
	}
	defer func() {
		for _, s := range states {
			s.Close()
		}
	}()

	runtime.GC()
	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)

	heapDelta := int64(mAfter.HeapAlloc) - int64(mBefore.HeapAlloc)
	if heapDelta < 0 {
		heapDelta = int64(mAfter.HeapAlloc)
	}

	b.ResetTimer()
	for b.Loop() {
		runtime.GC()
	}
	b.ReportMetric(float64(heapDelta)/float64(numVMs)/1024.0, "heap-KiB/VM")
	b.ReportMetric(float64(totalBacking)/float64(numVMs)/1024.0, "arena-KiB/VM")
	b.ReportMetric(float64(totalAlloc)/float64(numVMs)/1024.0, "live-KiB/VM")
	b.ReportMetric(float64(totalLiveObjs)/float64(numVMs), "lua-objs/VM")
	runtime.KeepAlive(states)
}
