package hook

import "testing"

func TestRegistryEmit(t *testing.T) {
	r := New[int]()
	var got []int
	r.Register(func(v int) { got = append(got, v) })
	r.Register(func(v int) { got = append(got, v*10) })
	r.Emit(2)
	if len(got) != 2 || got[0] != 2 || got[1] != 20 {
		t.Fatalf("got %v", got)
	}
	if r.Len() != 2 {
		t.Fatalf("len = %d", r.Len())
	}
}

func TestRegistryPanicIsolated(t *testing.T) {
	r := New[int]()
	r.Register(func(int) { panic("boom") })
	after := false
	r.Register(func(int) { after = true })
	r.Emit(1) // must not propagate
	if !after {
		t.Fatal("subscriber after a panicking one was not called")
	}
}
