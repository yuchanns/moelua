package moelua_test

import (
	"fmt"
	"testing/fstest"

	"github.com/yuchanns/moelua"
)

func ExampleWithLibs() {
	// Open only compute-safe standard libraries plus a sandboxed require() over an in-memory FS.
	vfs := fstest.MapFS{
		"greeter.lua": {Data: []byte(`return { hi = function(name) return "hello, " .. name end }`)},
	}
	s := moelua.New(
		moelua.WithLibs(moelua.LibSafe|moelua.LibPackageSafe),
		moelua.WithFS(vfs),
	)
	defer s.Close()

	res, err := s.DoString(`
		local g = require("greeter")
		return g.hi("moelua"), os == nil, io == nil
	`)
	if err != nil {
		panic(err)
	}
	msg, _ := s.ToString(res[0])
	fmt.Println(msg, res[1].IsTrue(), res[2].IsTrue())
	// Output: hello, moelua true true
}

func ExampleNewTemplate() {
	// Build a reusable sandboxed snapshot once with a custom host library and Lua helper.
	tmpl, err := moelua.NewTemplate(func(s *moelua.State) error {
		if _, err := s.RequireLib("svc", func(st *moelua.State, _ []moelua.Value) ([]moelua.Value, error) {
			mod := st.NewTable(0, 1)
			_ = mod.SetString("id", st.NewFunc1("svc.id", func(cur *moelua.State, _ []moelua.Value) (moelua.Value, error) {
				return cur.GetGlobal("worker_id"), nil
			}))
			return []moelua.Value{mod.Value()}, nil
		}, true); err != nil {
			return err
		}
		_, err := s.DoString(`function label() return "worker:" .. svc.id() end`)
		return err
	}, moelua.WithLibs(moelua.LibSafe|moelua.LibPackageSafe))
	if err != nil {
		panic(err)
	}

	// Clone isolated worker States in microseconds without repeating setup.
	s := tmpl.NewState(moelua.WithInstructionLimit(100_000))
	defer s.Close()
	s.SetGlobal("worker_id", s.String("alpha"))

	res, err := s.DoString(`return label()`)
	if err != nil {
		panic(err)
	}
	out, _ := s.ToString(res[0])
	fmt.Println(out)
	// Output: worker:alpha
}
