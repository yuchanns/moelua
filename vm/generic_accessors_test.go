package vm

import "testing"

func TestStateGenericHostAndUserdataData(t *testing.T) {
	type hostBox struct{ name string }
	type udBox struct{ count int }

	var nilState *State
	if _, ok := nilState.Host[*hostBox](); ok {
		t.Fatalf("nil State.Host returned ok=true")
	}
	if _, ok := nilState.UserdataData[*udBox](nil); ok {
		t.Fatalf("nil State.UserdataData returned ok=true")
	}

	s := NewStateWithLibs(LibNone, true)
	defer s.Close()

	if _, ok := s.Host[*hostBox](); ok {
		t.Fatalf("unset Host returned ok=true")
	}
	h := &hostBox{name: "alpha"}
	s.SetHost(h)
	if got, ok := s.Host[*hostBox](); !ok || got != h {
		t.Fatalf("Host[*hostBox]() = (%v, %v), want (%v, true)", got, ok, h)
	}
	if _, ok := s.Host[string](); ok {
		t.Fatalf("Host[string]() unexpectedly matched *hostBox")
	}
	s.SetHost(nil)
	if _, ok := s.Host[*hostBox](); ok {
		t.Fatalf("cleared Host returned ok=true")
	}

	if _, ok := s.UserdataData[*udBox](nil); ok {
		t.Fatalf("UserdataData(nil) returned ok=true")
	}

	box := &udBox{count: 42}
	udVal, err := s.TryNewUserdata(box, 0)
	if err != nil {
		t.Fatalf("TryNewUserdata: %v", err)
	}
	ud := s.AsUserdata(udVal)
	if got, ok := s.UserdataData[*udBox](ud); !ok || got != box {
		t.Fatalf("UserdataData[*udBox] = (%v, %v), want (%v, true)", got, ok, box)
	}
	if got, ok := s.UserdataData[any](ud); !ok || got != box {
		t.Fatalf("UserdataData[any] = (%v, %v), want (%v, true)", got, ok, box)
	}
	if _, ok := s.UserdataData[string](ud); ok {
		t.Fatalf("UserdataData[string] unexpectedly matched *udBox")
	}

	nilUDVal, err := s.TryNewUserdata(nil, 0)
	if err != nil {
		t.Fatalf("TryNewUserdata(nil): %v", err)
	}
	nilUD := s.AsUserdata(nilUDVal)
	if _, ok := s.UserdataData[*udBox](nilUD); ok {
		t.Fatalf("UserdataData[*udBox](nil payload) returned ok=true")
	}
	if _, ok := s.UserdataData[any](nilUD); ok {
		t.Fatalf("UserdataData[any](nil payload) returned ok=true")
	}
}
