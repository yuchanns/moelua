package bench_test

import (
	"testing"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/vm"
)

func compileChunk(b *testing.B, src string) *bytecode.Proto {
	b.Helper()
	p, err := compiler.Compile("=bench", src)
	if err != nil {
		b.Fatalf("compile failed: %v", err)
	}
	return p
}

// Class (d): Go-host embedding microbenchmarks
func BenchmarkNewState(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		s := vm.NewState()
		_ = s
	}
}

func BenchmarkGoCallLua(b *testing.B) {
	proto := compileChunk(b, `return function(a, b) return a * 3 + b end`)
	s := vm.NewState()
	cl, err := s.Load(proto)
	if err != nil {
		b.Fatal(err)
	}
	res, err := s.Call(vm.ClosureValue(cl))
	if err != nil {
		b.Fatal(err)
	}
	fn := res[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		out, err := s.Call(fn, vm.IntValue(int64(i)), vm.IntValue(7))
		if err != nil || out[0].Int() != int64(i)*3+7 {
			b.Fatalf("unexpected: %v %v", out, err)
		}
	}
}

func BenchmarkLuaCallGoHost1M(b *testing.B) {
	proto := compileChunk(b, `
local host_add = host_add
return function(n)
  local acc = 0
  for i = 1, n do
    acc = host_add(acc, i)
  end
  return acc
end
`)
	s := vm.NewState()
	s.SetGlobal("host_add", vm.NativeValue(&vm.NativeFunc{
		Name: "host_add",
		Fn: func(_ *vm.State, args []vm.Value) ([]vm.Value, error) {
			return []vm.Value{vm.IntValue(args[0].Int() + args[1].Int())}, nil
		},
	}))
	cl, err := s.Load(proto)
	if err != nil {
		b.Fatal(err)
	}
	res, err := s.Call(vm.ClosureValue(cl))
	if err != nil {
		b.Fatal(err)
	}
	fn := res[0]
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := s.Call(fn, vm.IntValue(1000000))
		if err != nil || out[0].Int() != 500000500000 {
			b.Fatalf("unexpected: %v %v", out, err)
		}
	}
}

func BenchmarkLuaCallGoHostFast1M(b *testing.B) {
	proto := compileChunk(b, `
local host_add = host_add
return function(n)
  local acc = 0
  for i = 1, n do
    acc = host_add(acc, i)
  end
  return acc
end
`)
	s := vm.NewState()
	s.SetGlobal("host_add", vm.NativeValue(&vm.NativeFunc{
		Name: "host_add",
		FastFn: func(_ *vm.State, args []vm.Value) (vm.Value, error) {
			return vm.IntValue(args[0].Int() + args[1].Int()), nil
		},
	}))
	cl, err := s.Load(proto)
	if err != nil {
		b.Fatal(err)
	}
	res, err := s.Call(vm.ClosureValue(cl))
	if err != nil {
		b.Fatal(err)
	}
	fn := res[0]
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := s.Call(fn, vm.IntValue(1000000))
		if err != nil || out[0].Int() != 500000500000 {
			b.Fatalf("unexpected: %v %v", out, err)
		}
	}
}
