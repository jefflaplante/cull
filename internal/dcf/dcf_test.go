package dcf

import (
	"math/rand"
	"slices"
	"sort"
	"testing"
)

func names(ps ...string) []Name {
	out := make([]Name, len(ps))
	for i, p := range ps {
		out[i] = Of(p)
	}
	return out
}

func bases(ns []Name) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Base
	}
	return out
}

func sorted(ns []Name) []Name {
	out := slices.Clone(ns)
	Unify(out)
	sort.SliceStable(out, func(a, b int) bool { return Less(out[a], out[b]) })
	return out
}

func TestOf(t *testing.T) {
	for p, want := range map[string]Name{
		"M1103127.DNG":                          {Base: "M1103127.DNG"},
		"100LEICA/M1103127.DNG":                 {Folder: 100, Base: "M1103127.DNG"},
		"/Volumes/LEICA M/DCIM/101LEICA/L1.DNG": {Folder: 101, Base: "L1.DNG"},
		"/shoots/2026-10-04 x/M1103127.DNG":     {Base: "M1103127.DNG"},
		"/a/100canon/IMG_0001.CR3":              {Folder: 100, Base: "IMG_0001.CR3"},
		"/a/099LEICA/M1.DNG":                    {Base: "M1.DNG"}, // below 100: not DCF
		"/a/100LEI-A/M1.DNG":                    {Base: "M1.DNG"}, // '-' isn't a DCF character
		"/a/1000LEICA/M1.DNG":                   {Base: "M1.DNG"}, // 9 characters
	} {
		if got := Of(p); got != want {
			t.Errorf("Of(%q) = %+v, want %+v", p, got, want)
		}
	}
}

func TestCounter(t *testing.T) {
	for base, want := range map[string]int{
		"M1102767.DNG": 2767, "l1002772.dng": 2772, "IMG_1234.JPG": 1234, "DSC01234.ARW": 1234,
		"M1102767": 2767, "_DSC0001.NEF": 1,
		"M110276.DNG": -1, "M11027670.DNG": -1, "M110x767.DNG": -1, "M-102767.DNG": -1,
		"20261004_0001.DNG": -1, "s_1.DNG": -1, "": -1, "M1.DNG": -1,
	} {
		if got := counter(base); got != want {
			t.Errorf("counter(%q) = %d, want %d", base, got, want)
		}
	}
}

// The M11-P shares one counter across its M… and L… (Content Credentials) names:
// camera order follows the counter, not the letters.
func TestSharedCounter(t *testing.T) {
	in := names("L1002774.DNG", "M1102769.DNG", "L1002772.DNG", "M1102771.DNG", "L1002776.DNG",
		"M1102767.DNG", "L1002773.DNG", "M1102770.DNG", "L1002775.DNG", "M1102768.DNG")
	want := []string{"M1102767.DNG", "M1102768.DNG", "M1102769.DNG", "M1102770.DNG", "M1102771.DNG",
		"L1002772.DNG", "L1002773.DNG", "L1002774.DNG", "L1002775.DNG", "L1002776.DNG"}
	if got := bases(sorted(in)); !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
}

// The counter wraps past 9999 into the next folder: the folder number orders first
// when every frame's folder is known, and is ignored when any isn't.
func TestFolderWrap(t *testing.T) {
	in := names("101LEICA/IMG_0001.DNG", "100LEICA/IMG_9999.DNG", "100LEICA/IMG_9998.DNG", "101LEICA/IMG_0002.DNG")
	if got := bases(sorted(in)); !slices.Equal(got, []string{"IMG_9998.DNG", "IMG_9999.DNG", "IMG_0001.DNG", "IMG_0002.DNG"}) {
		t.Fatalf("known folders: %v", got)
	}
	in = append(in, Of("IMG_5000.DNG")) // one unknown folder: counters alone
	if got := bases(sorted(in)); !slices.Equal(got, []string{"IMG_0001.DNG", "IMG_0002.DNG", "IMG_5000.DNG", "IMG_9998.DNG", "IMG_9999.DNG"}) {
		t.Fatalf("an unknown folder: %v", got)
	}
}

// A single-prefix shoot keeps its name order.
func TestSinglePrefixIsNameOrder(t *testing.T) {
	var in []Name
	var want []string
	for n := 3127; n <= 4130; n++ {
		b := "M110" + itoa4(n) + ".DNG"
		want = append(want, b)
		in = append(in, Of("/Volumes/LEICA M/DCIM/100LEICA/"+b))
	}
	rand.New(rand.NewSource(1)).Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
	if got := bases(sorted(in)); !slices.Equal(got, want) {
		t.Fatal("order changed")
	}
}

func itoa4(n int) string {
	b := []byte("0000")
	for i := 3; i >= 0; i-- {
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b)
}

// Names without the DCF shape come after DCF names, by name ignoring case, then by
// exact name; case doesn't matter to the DCF shape.
func TestNonDCFAndCase(t *testing.T) {
	in := names("s_2.DNG", "m1102768.dng", "S_10.DNG", "M1102767.DNG", "a.DNG", "A.DNG", "L1002769.DNG")
	want := []string{"M1102767.DNG", "m1102768.dng", "L1002769.DNG", "A.DNG", "a.DNG", "S_10.DNG", "s_2.DNG"}
	if got := bases(sorted(in)); !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
	// The same counter in two names: by name ignoring case, then exactly.
	in = names("M1100001.DNG", "L1000001.DNG", "l1000001.DNG")
	if got := bases(sorted(in)); !slices.Equal(got, []string{"L1000001.DNG", "l1000001.DNG", "M1100001.DNG"}) {
		t.Fatalf("got %v", got)
	}
}

// Compare is a total order on any names (unified or not): antisymmetric, transitive,
// and 0 only for names that are the same in camera order. Sorting any shuffle gives
// one sequence.
func TestCompareTotal(t *testing.T) {
	pool := []string{"M1102767.DNG", "m1102767.DNG", "L1002767.DNG", "M1109999.DNG", "IMG_0001.JPG", "IMG_0001.jpg",
		"s_1.DNG", "S_1.DNG", "s_10.DNG", "a.DNG", "", "M1.DNG", "DSC00042.ARW"}
	folders := []int{0, 100, 101, 999}
	r := rand.New(rand.NewSource(7))
	pick := func() Name {
		n := Of(pool[r.Intn(len(pool))])
		if n.Base != "" {
			n.Folder = folders[r.Intn(len(folders))]
		}
		return n
	}
	sign := func(x int) int { return min(1, max(-1, x)) }
	for i := 0; i < 20000; i++ {
		a, b, c := pick(), pick(), pick()
		if Compare(a, a) != 0 {
			t.Fatalf("Compare(%v, itself) != 0", a)
		}
		if sign(Compare(a, b)) != -sign(Compare(b, a)) {
			t.Fatalf("not antisymmetric: %v %v", a, b)
		}
		if Compare(a, b) <= 0 && Compare(b, c) <= 0 && Compare(a, c) > 0 {
			t.Fatalf("not transitive: %v %v %v", a, b, c)
		}
		if Compare(a, b) == 0 && Compare(a, c) != Compare(b, c) {
			t.Fatalf("0 isn't an equivalence: %v %v %v", a, b, c)
		}
	}
	var set []Name
	for i := 0; i < 40; i++ {
		set = append(set, pick())
	}
	first := sorted(set)
	for i := 0; i < 50; i++ {
		r.Shuffle(len(set), func(i, j int) { set[i], set[j] = set[j], set[i] })
		got := sorted(set)
		for k := range got {
			if Compare(got[k], first[k]) != 0 {
				t.Fatalf("shuffle %d sorted differently at %d: %v vs %v", i, k, got[k], first[k])
			}
		}
	}
}
