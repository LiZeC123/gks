package metrics

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestNew(t *testing.T) {
	rb := NewRingBuffer(3)
	got := rb.Snapshot()
	want := []string{"", "", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot() = %v, want %v", got, want)
	}
}

func TestNewPanicsOnNonPositiveCapacity(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("New(%d) did not panic", n)
				}
			}()
			NewRingBuffer(n)
		})
	}
}

func TestWriteAndSnapshot(t *testing.T) {
	tests := []struct {
		name   string
		n      int
		writes []string
		want   []string
	}{
		{
			name:   "empty",
			n:      3,
			writes: nil,
			want:   []string{"", "", ""},
		},
		{
			name:   "single write",
			n:      3,
			writes: []string{"a"},
			want:   []string{"", "", "a"},
		},
		{
			name:   "partial fill",
			n:      3,
			writes: []string{"a", "b"},
			want:   []string{"", "a", "b"},
		},
		{
			name:   "exactly full",
			n:      3,
			writes: []string{"a", "b", "c"},
			want:   []string{"a", "b", "c"},
		},
		{
			name:   "overwrite once",
			n:      3,
			writes: []string{"a", "b", "c", "d"},
			want:   []string{"b", "c", "d"},
		},
		{
			name:   "overwrite multiple times",
			n:      3,
			writes: []string{"a", "b", "c", "d", "e", "f", "g"},
			want:   []string{"e", "f", "g"},
		},
		{
			name:   "n=1 single write",
			n:      1,
			writes: []string{"a"},
			want:   []string{"a"},
		},
		{
			name:   "n=1 overwrite",
			n:      1,
			writes: []string{"a", "b", "c"},
			want:   []string{"c"},
		},
		{
			name:   "n=5 no wrap",
			n:      5,
			writes: []string{"1", "2", "3", "4", "5"},
			want:   []string{"1", "2", "3", "4", "5"},
		},
		{
			name:   "n=5 wrapped",
			n:      5,
			writes: []string{"1", "2", "3", "4", "5", "6", "7"},
			want:   []string{"3", "4", "5", "6", "7"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			rb := NewRingBuffer(tt.n)
			for _, s := range tt.writes {
				rb.Write(s)
			}

			got := rb.Snapshot()
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Snapshot() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSnapshotReturnsIndependentCopy(t *testing.T) {
	rb := NewRingBuffer(3)
	rb.Write("a")
	rb.Write("b")

	snap := rb.Snapshot()
	snap[0] = "hacked"
	snap[1] = "hacked"
	snap[2] = "hacked"

	got := rb.Snapshot()
	want := []string{"", "a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("internal buffer was modified by caller: got %v, want %v", got, want)
	}
}

func TestSnapshotLengthAlwaysEqualsCapacity(t *testing.T) {
	for _, n := range []int{1, 2, 10, 100} {
		rb := NewRingBuffer(n)
		for i := 0; i < n*3; i++ {
			rb.Write(fmt.Sprintf("%d", i))
			if got := len(rb.Snapshot()); got != n {
				t.Fatalf("n=%d, Snapshot len = %d, want %d", n, got, n)
			}
		}
	}
}

func TestConcurrentWrites(t *testing.T) {
	const (
		capacity   = 100
		goroutines = 10
		writesPerG = 1000
	)

	rb := NewRingBuffer(capacity)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < writesPerG; i++ {
				rb.Write(fmt.Sprintf("g%d-%d", g, i))
			}
		}(g)
	}
	wg.Wait()

	snap := rb.Snapshot()
	if len(snap) != capacity {
		t.Fatalf("Snapshot len = %d, want %d", len(snap), capacity)
	}

	// 由于每次写入的字符串都唯一，且总写入次数远大于容量，
	// 快照中应包含 capacity 个互不相同、非空的字符串。
	seen := make(map[string]struct{}, capacity)
	for _, s := range snap {
		if s == "" {
			t.Fatalf("unexpected empty string in snapshot: %v", snap)
		}
		if _, ok := seen[s]; ok {
			t.Fatalf("duplicate string %q in snapshot: %v", s, snap)
		}
		seen[s] = struct{}{}
	}
}

func TestConcurrentWriteAndSnapshot(t *testing.T) {
	const capacity = 50

	rb := NewRingBuffer(capacity)
	var writers sync.WaitGroup
	var readers sync.WaitGroup
	stop := make(chan struct{})

	for g := 0; g < 4; g++ {
		writers.Add(1)
		go func(g int) {
			defer writers.Done()
			for i := 0; i < 2000; i++ {
				rb.Write(fmt.Sprintf("g%d-%d", g, i))
			}
		}(g)
	}

	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					snap := rb.Snapshot()
					if len(snap) != capacity {
						t.Errorf("Snapshot len = %d, want %d", len(snap), capacity)
						return
					}
				}
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()
}
