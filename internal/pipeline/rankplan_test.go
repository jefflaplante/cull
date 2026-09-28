package pipeline

import (
	"reflect"
	"testing"
)

func TestChunksAndFinalists(t *testing.T) {
	for _, c := range []struct{ n, keep, chunks, per int }{
		{8, 3, 1, 8}, {9, 3, 2, 4}, {16, 3, 2, 4}, {40, 3, 5, 1}, {40, 5, 5, 1}, {40, 8, 5, 2},
	} {
		ch := chunks(c.n)
		total := 0
		for _, x := range ch {
			total += len(x)
			if len(x) > maxRankFrames {
				t.Errorf("n=%d: chunk of %d", c.n, len(x))
			}
		}
		if len(ch) != c.chunks || total != c.n {
			t.Errorf("n=%d: %d chunks covering %d", c.n, len(ch), total)
		}
		if f := finalists(len(ch), c.keep); c.chunks > 1 && f != c.per {
			t.Errorf("n=%d keep=%d: finalists %d, want %d", c.n, c.keep, f, c.per)
		}
		if f := finalists(len(ch), c.keep); c.chunks > 1 && f*len(ch) < c.keep {
			t.Errorf("n=%d keep=%d: final round smaller than KeepBest", c.n, c.keep)
		}
	}
}

func TestMergeOrder(t *testing.T) {
	// Two chunks: positions 0-3 ranked [2,0,3,1], 4-7 ranked [5,7,4,6]; 2 finalists each
	// → finals among {2,0,5,7} ranked [5,2,7,0].
	got := merge([][]int{{2, 0, 3, 1}, {5, 7, 4, 6}}, []int{5, 2, 7, 0})
	want := []int{5, 2, 7, 0, 3, 4, 1, 6} // finals, then chunk rank 3s (3 before 4), then rank 4s
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}
