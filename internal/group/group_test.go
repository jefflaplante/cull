package group

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/jefflaplante/cull/internal/dcf"
)

func seqFrame(key string, sec int, look []uint8) Frame {
	return Frame{Key: key, Time: time.Unix(1_800_000_000+int64(sec), 0), HasTime: true, Look: look}
}

func TestSequencesLinkByGapAndLookToPrevious(t *testing.T) {
	a := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	drift := func(i int) []uint8 { return scene(park, coat, 0.5, 0.5, 6*i, 0, 1, 1) } // walks away slowly
	frames := []Frame{seqFrame("L1", 0, a)}
	for i := 1; i < 10; i++ {
		frames = append(frames, seqFrame(fmt.Sprintf("L%d", i+1), 20*i, drift(i)))
	}
	frames = append(frames, seqFrame("L11", 400, a)) // same look, but 220 s later: new set
	sets := Sequences(frames, Options{Gap: 60 * time.Second, MaxLook: 0.08})
	if len(sets) != 1 || len(sets[0]) != 10 {
		t.Fatalf("sets %v: the 10 drifting frames link via their neighbours; the late one stands alone", sets)
	}
}

// A subject walking across a fixed view: each frame looks like the one before it,
// but the last looks nothing like the first. Linking to the previous frame keeps
// the walk in one set.
func TestSequencesFollowASubjectAcrossTheFrame(t *testing.T) {
	var frames []Frame
	for i := 0; i < 10; i++ {
		frames = append(frames, seqFrame(fmt.Sprintf("L%d", i+1), 2*i, scene(park, coat, 0.2+0.04*float64(i), 0.5, 0, 0, 1, 1)))
	}
	o := Options{Gap: 60 * time.Second, MaxLook: 0.08}
	if d := LookDistance(frames[0].Look, frames[9].Look); d <= o.MaxLook {
		t.Fatalf("premise: the first and last frames must not link directly (%.3f)", d)
	}
	if sets := Sequences(frames, o); len(sets) != 1 || len(sets[0]) != 10 {
		t.Fatalf("sets %v", sets)
	}
}

func TestSequencesSplitDifferentScenes(t *testing.T) {
	frames := []Frame{
		seqFrame("L1", 0, scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)),
		seqFrame("L2", 5, scene(park, coat, 0.52, 0.5, 0, 0, 1, 1)),
		seqFrame("L3", 10, scene(wall, shirt, 0.3, 0.6, 0, 0, 1, 1)),
		seqFrame("L4", 15, scene(wall, shirt, 0.31, 0.6, 0, 0, 1, 1)),
		seqFrame("L5", 20, scene(park, shirt, 0.3, 0.5, 0, 0, 1, 1)), // same place, different subject
	}
	sets := Sequences(frames, Options{Gap: 60 * time.Second, MaxLook: 0.08})
	if len(sets) != 2 || len(sets[0]) != 2 || len(sets[1]) != 2 {
		t.Fatalf("sets %v", sets)
	}
}

func TestSequencesCapAndOrdering(t *testing.T) {
	look := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	var frames []Frame
	for i := 0; i < 45; i++ {
		frames = append(frames, Frame{Key: fmt.Sprintf("L%03d", 45-i), Look: look}) // no times, reverse input order
	}
	sets := Sequences(frames, Options{Gap: 60 * time.Second, MaxLook: 0.08})
	if len(sets) != 2 || len(sets[0]) != MaxSequence || len(sets[1]) != 5 {
		t.Fatalf("cap: %d sets, sizes %d/%d", len(sets), len(sets[0]), len(sets[len(sets)-1]))
	}
	if frames[sets[0][0]].Key != "L001" {
		t.Fatalf("untimed frames go in file-name order, got %s first", frames[sets[0][0]].Key)
	}
	if Sequences(frames, Options{Gap: 0, MaxLook: 0.08}) != nil {
		t.Fatal("Gap 0 disables grouping")
	}
}

func TestSequencesOfNothing(t *testing.T) {
	o := Options{Gap: time.Minute, MaxLook: 0.08}
	if s := Sequences(nil, o); s != nil {
		t.Fatalf("got %v", s)
	}
	if s := Sequences([]Frame{{Key: "a", Look: scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)}}, o); s != nil {
		t.Fatalf("one frame is not a set: %v", s)
	}
	if s := Sequences([]Frame{{Key: "a"}, {Key: "b"}}, o); s != nil {
		t.Fatalf("frames without a look never link: %v", s)
	}
}

func TestScoreOrder(t *testing.T) {
	frames := []Frame{
		{Key: "a", Score: Score{Evaluated: true, Sharp: 7}},
		{Key: "b", Score: Score{Evaluated: true, Sharp: 9}},
		{Key: "c", Score: Score{Evaluated: true, Sharp: 9, EyesOpen: true}},
	}
	if got := ScoreOrder(frames, []int{0, 1, 2}); !reflect.DeepEqual(got, []int{2, 1, 0}) {
		t.Fatalf("%v", got)
	}
}

func TestScoreOrderPrefersEvaluatedThenCompositionThenCaptureOrder(t *testing.T) {
	frames := []Frame{
		{Key: "a", Score: Score{Evaluated: false}},
		{Key: "b", Score: Score{Evaluated: true, Sharp: 8, EyesOpen: true, Comp: 4}},
		{Key: "c", Score: Score{Evaluated: true, Sharp: 8, EyesOpen: true, Comp: 6}},
		{Key: "d", Score: Score{Evaluated: true, Sharp: 8, EyesOpen: true, Comp: 6}},
	}
	if got := ScoreOrder(frames, []int{0, 1, 2, 3}); !reflect.DeepEqual(got, []int{2, 3, 1, 0}) {
		t.Fatalf("%v: want c, d (tie: capture order), b, then the unevaluated a", got)
	}
}

// scene renders a synthetic 320×240 "photo": a background gradient with a subject
// block. dx/dy shift the whole view, gain scales brightness, zoom scales about
// the centre.
func scene(bg, subj [3]float64, sx, sy float64, dx, dy int, gain, zoom float64) []uint8 {
	const w, h = 320, 240
	grid := make([]uint8, LookSize*LookSize*3)
	for cy := 0; cy < LookSize; cy++ {
		for cx := 0; cx < LookSize; cx++ {
			var acc [3]float64
			n := 0
			for y := cy * h / LookSize; y < (cy+1)*h/LookSize; y += 4 {
				for x := cx * w / LookSize; x < (cx+1)*w/LookSize; x += 4 {
					u := (float64(x-w/2)/zoom + float64(w/2) + float64(dx)) / w
					v := (float64(y-h/2)/zoom + float64(h/2) + float64(dy)) / h
					c := bg
					for k := range c {
						c[k] = bg[k] * (0.6 + 0.4*v)
					}
					if math.Abs(u-sx) < 0.12 && math.Abs(v-sy) < 0.2 {
						c = subj
					}
					for k := range acc {
						acc[k] += math.Min(255, c[k]*gain)
					}
					n++
				}
			}
			for k := range acc {
				grid[(cy*LookSize+cx)*3+k] = uint8(acc[k] / float64(n))
			}
		}
	}
	return grid
}

var (
	park  = [3]float64{70, 140, 60}
	coat  = [3]float64{200, 60, 40}
	wall  = [3]float64{180, 170, 150}
	shirt = [3]float64{40, 60, 160}
)

func TestLookDistanceToleratesReframingAndExposure(t *testing.T) {
	base := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	for name, other := range map[string][]uint8{
		"shift 10%":     scene(park, coat, 0.5, 0.5, 32, 0, 1, 1),
		"one stop up":   scene(park, coat, 0.5, 0.5, 0, 0, 1.6, 1),
		"zoom 5%":       scene(park, coat, 0.5, 0.5, 0, 0, 1, 1.05),
		"subject moved": scene(park, coat, 0.56, 0.5, 0, 0, 1, 1),
	} {
		if d := LookDistance(base, other); d > 0.08 {
			t.Errorf("%s: distance %.3f, want ≤ 0.08", name, d)
		}
	}
}

func TestLookDistanceSeparatesScenes(t *testing.T) {
	base := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	for name, other := range map[string][]uint8{
		"different scene":               scene(wall, shirt, 0.3, 0.6, 0, 0, 1, 1),
		"same place, different subject": scene(park, shirt, 0.3, 0.5, 0, 0, 1, 1),
	} {
		if d := LookDistance(base, other); d < 0.12 {
			t.Errorf("%s: distance %.3f, want ≥ 0.12", name, d)
		}
	}
	if d := LookDistance(base, base); d != 0 {
		t.Errorf("identical: %.3f", d)
	}
}

// A hand-edited or truncated report must not crash decide: a malformed look is
// as far as looks go.
func TestLookDistanceRejectsMalformedLooks(t *testing.T) {
	good := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	long := append(append([]uint8(nil), good...), 1, 2, 3)
	for name, pair := range map[string][2][]uint8{
		"truncated":     {good, good[:100]},
		"missing":       {nil, good},
		"both short":    {good[:3], good[:3]},
		"both too long": {long, long},
	} {
		if d := LookDistance(pair[0], pair[1]); d != 1 {
			t.Errorf("%s: distance %v, want 1", name, d)
		}
	}
}

// Frames with one capture time (a burst, or a camera clock that wasn't running) are
// in camera order: the M11-P numbers its M… and L… frames from one counter, so
// M1102771 comes before L1002772 although L sorts first by name.
func TestOrderTiesByCameraCounter(t *testing.T) {
	look := scene(park, coat, 0.5, 0.5, 0, 0, 1, 1)
	var frames []Frame
	for _, n := range []string{"L1002773.DNG", "M1102770.DNG", "L1002772.DNG", "M1102771.DNG"} {
		f := seqFrame("/shoot/"+n, 0, look)
		f.Name = dcf.Of(n)
		frames = append(frames, f)
	}
	want := []int{1, 3, 2, 0} // M1102770 M1102771 L1002772 L1002773
	if got := Order(frames); !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	if sets := Sequences(frames, Options{Gap: time.Minute, MaxLook: DefaultLook}); len(sets) != 1 || !reflect.DeepEqual(sets[0], want) {
		t.Fatalf("sets %v", sets)
	}
	// Without capture times, the same.
	for i := range frames {
		frames[i].HasTime = false
	}
	if got := Order(frames); !reflect.DeepEqual(got, want) {
		t.Fatalf("untimed order %v, want %v", got, want)
	}
	// Capture time still comes first.
	frames[3].HasTime, frames[0].HasTime, frames[1].HasTime, frames[2].HasTime = true, true, true, true
	frames[1].Time = frames[1].Time.Add(time.Second) // M1102770 a second later
	if got := Order(frames); !reflect.DeepEqual(got, []int{3, 2, 0, 1}) {
		t.Fatalf("timed order %v", got)
	}
}
