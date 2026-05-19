package sandbox

import (
	"errors"
	"sync"
	"testing"
)

func TestAllocator_AllocateReleaseRoundTrip(t *testing.T) {
	a := newAllocator()
	idx, err := a.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if a.IsFree(idx) {
		t.Errorf("idx %d still marked free after Allocate", idx)
	}
	a.Release(idx)
	if !a.IsFree(idx) {
		t.Errorf("idx %d not marked free after Release", idx)
	}
}

func TestAllocator_ExhaustsAt256(t *testing.T) {
	a := newAllocator()
	for i := 0; i < 256; i++ {
		if _, err := a.Allocate(); err != nil {
			t.Fatalf("Allocate #%d: %v", i, err)
		}
	}
	_, err := a.Allocate()
	if !errors.Is(err, ErrSubnetsExhausted) {
		t.Errorf("257th Allocate err = %v, want ErrSubnetsExhausted", err)
	}
}

func TestAllocator_RecyclesReleasedSlot(t *testing.T) {
	a := newAllocator()
	idxs := make([]uint8, 256)
	for i := 0; i < 256; i++ {
		idx, err := a.Allocate()
		if err != nil {
			t.Fatalf("Allocate #%d: %v", i, err)
		}
		idxs[i] = idx
	}
	a.Release(idxs[42])
	idx, err := a.Allocate()
	if err != nil {
		t.Fatalf("Allocate after Release: %v", err)
	}
	if idx != idxs[42] {
		t.Errorf("recycled idx = %d, want %d", idx, idxs[42])
	}
}

func TestAllocator_ConcurrentSafe(t *testing.T) {
	a := newAllocator()
	const N = 256
	got := make(chan uint8, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx, err := a.Allocate()
			if err != nil {
				t.Errorf("Allocate: %v", err)
				return
			}
			got <- idx
		}()
	}
	wg.Wait()
	close(got)
	seen := make(map[uint8]bool, N)
	for idx := range got {
		if seen[idx] {
			t.Errorf("duplicate idx %d allocated concurrently", idx)
		}
		seen[idx] = true
	}
	if len(seen) != N {
		t.Errorf("got %d unique idxs, want %d", len(seen), N)
	}
}

func TestSubnetHelpers(t *testing.T) {
	if got := subnetCIDR(7); got != "10.42.7.0/24" {
		t.Errorf("subnetCIDR(7) = %q, want 10.42.7.0/24", got)
	}
	if got := hostIP(7); got != "10.42.7.1" {
		t.Errorf("hostIP(7) = %q, want 10.42.7.1", got)
	}
	if got := sandboxIP(7); got != "10.42.7.2" {
		t.Errorf("sandboxIP(7) = %q, want 10.42.7.2", got)
	}
}

func TestParseSubnetIdx(t *testing.T) {
	cases := []struct {
		ip      string
		wantIdx uint8
		wantOK  bool
	}{
		{"10.42.7.1", 7, true},
		{"10.42.0.1", 0, true},
		{"10.42.255.2", 255, true},
		{"10.43.7.1", 0, false},    // wrong second octet
		{"192.168.99.1", 0, false}, // entirely different range
		{"not-an-ip", 0, false},
	}
	for _, c := range cases {
		idx, ok := parseSubnetIdx(c.ip)
		if ok != c.wantOK || idx != c.wantIdx {
			t.Errorf("parseSubnetIdx(%q) = (%d, %v), want (%d, %v)",
				c.ip, idx, ok, c.wantIdx, c.wantOK)
		}
	}
}

func TestSubnetIdxFromNetnsName(t *testing.T) {
	cases := []struct {
		name    string
		wantIdx uint8
		wantOK  bool
	}{
		{"tf-abc123def-7", 7, true},
		{"tf-deadbeef-255", 255, true},
		{"tf-aaaa-0", 0, true},
		{"tf-uppercase-7", 0, false},     // non-hex
		{"other-prefix-7", 0, false},     // wrong prefix
		{"tf-abc", 0, false},             // no idx suffix
		{"tf-abc-256", 0, false},         // out of uint8 range
		{"tf-myown-something", 0, false}, // collateral-damage guard (matches "tf-*" but not our pattern)
	}
	for _, c := range cases {
		idx, ok := subnetIdxFromNetnsName(c.name)
		if ok != c.wantOK || idx != c.wantIdx {
			t.Errorf("subnetIdxFromNetnsName(%q) = (%d, %v), want (%d, %v)",
				c.name, idx, ok, c.wantIdx, c.wantOK)
		}
	}
}
