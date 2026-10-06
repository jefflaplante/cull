package offload

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// readOnlyCard makes every frame on the card read-only (0444), so anything that opened a
// card file for writing, or mapped it writable, would fail with a permission error.
func readOnlyCard(t *testing.T, p *Plan) {
	t.Helper()
	for _, f := range p.Files {
		if err := os.Chmod(f.Src, 0o444); err != nil {
			t.Fatal(err)
		}
	}
}

// eventLog records, in order, each card open and each source eviction, as "open NAME"
// and "evict NAME".
type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	l.ev = append(l.ev, s)
	l.mu.Unlock()
}

// of is the events for name, in order, as "open"/"evict".
func (l *eventLog) of(name string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.ev {
		if verb, n, _ := strings.Cut(e, " "); n == name {
			out = append(out, verb)
		}
	}
	return strings.Join(out, ",")
}

func (l *eventLog) evictions() (n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.ev {
		if strings.HasPrefix(e, "evict ") {
			n++
		}
	}
	return n
}

// A retry is meant to read the card again, not RAM: before every retry attempt (never
// before try 0) the card file is evicted from the page cache, with the real, read-only
// eviction, on a card whose files are read-only. Twice in a row each: a card read error
// and a verify mismatch, pipelined and serial. The card is unchanged and the run safe.
func TestRetryEvictsSourceFirst(t *testing.T) {
	for _, serial := range []bool{false, true} {
		for _, failure := range []string{"read error", "verify mismatch"} {
			name := map[bool]string{false: "pipelined", true: "serial"}[serial] + ", " + failure
			t.Run(name, func(t *testing.T) {
				src, data := pipeCard(t, 6, rand.New(rand.NewSource(21)), func(i int) int { return 70000 + i })
				p := pipePlan(t, src)
				p.h.serial = serial
				readOnlyCard(t, p)
				before := treeHash(t, src)
				w := watchPlan(t, p, data)
				names := planNames(p)
				k := names[2]
				var log eventLog
				w.open = func(n string, try int) error {
					log.add("open " + n)
					if failure == "read error" && n == k && try <= 2 {
						return errors.New("input/output error")
					}
					return nil
				}
				if failure == "verify mismatch" {
					w.beforeVerify = func(tmp string, try int) {
						if strings.Contains(tmp, k) && filepath.Dir(tmp) == p.Dests[0] && try <= 2 {
							corrupt(t, tmp)
						}
					}
				}
				p.h.evictSource = func(s string) (int, int, error) {
					log.add("evict " + filepath.Base(s))
					return evictPasses(s) // the real thing: it must work on a read-only card
				}
				res, err := Run(context.Background(), p, nil)
				if err != nil || !res.Safe || res.Copied != len(names) || len(res.Failed) != 0 {
					t.Fatalf("err %v result %+v", err, res)
				}
				if got := log.of(k); got != "open,evict,open,evict,open" {
					t.Fatalf("%s: %s, want open,evict,open,evict,open (an eviction before each retry, none before try 0)", k, got)
				}
				if n := log.evictions(); n != 2 {
					t.Fatalf("%d evictions, want 2 (only %s was retried)", n, k)
				}
				recorded(t, p, data, names)
				if treeHash(t, src) != before {
					t.Fatal("the card changed")
				}
			})
		}
	}
}

// Eviction is best effort: if it fails, or pages stay cached, the retry still runs and
// the file still copies; one verbose note says so.
func TestRetryWhenSourceEvictionFails(t *testing.T) {
	for _, how := range []string{"error", "pages stay cached"} {
		t.Run(how, func(t *testing.T) {
			src, data := pipeCard(t, 4, rand.New(rand.NewSource(22)), func(i int) int { return 60000 + i })
			p := pipePlan(t, src)
			readOnlyCard(t, p)
			w := watchPlan(t, p, data)
			names := planNames(p)
			k := names[1]
			w.open = func(n string, try int) error {
				if n == k && try == 1 {
					return errors.New("input/output error")
				}
				return nil
			}
			evicts := 0
			p.h.evictSource = func(string) (int, int, error) {
				evicts++
				if how == "error" {
					return 0, 0, errors.New("mmap: operation not permitted")
				}
				return 7, 100, nil
			}
			sink := noteSink{notes: make(chan string, 1000)}
			res, err := Run(context.Background(), p, sink)
			close(sink.notes)
			if err != nil || !res.Safe || res.Copied != len(names) || len(res.Failed) != 0 {
				t.Fatalf("err %v result %+v", err, res)
			}
			if evicts != 1 || w.opened(k) != 2 {
				t.Fatalf("evictions %d, opens of %s %d; want 1 and 2", evicts, k, w.opened(k))
			}
			n := 0
			for s := range sink.notes {
				if strings.Contains(s, "page cache") {
					n++
					if !strings.Contains(s, k) {
						t.Errorf("note doesn't name the file: %q", s)
					}
					if how == "pages stay cached" && !strings.Contains(s, "7 of 100") {
						t.Errorf("note doesn't give the count: %q", s)
					}
				}
			}
			if n != 1 {
				t.Fatalf("%d notes about the page cache, want 1", n)
			}
			recorded(t, p, data, names)
		})
	}
}
