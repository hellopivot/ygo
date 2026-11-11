package crdt

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A merge of non-contiguous updates from one client carries a skip struct over
// the clocks it lacks. The decoder must park what follows the skip until those
// clocks arrive, as Yjs does, instead of moving the client's clock past the
// gap: that renumbered the parked structs into the gap and discarded the
// update that later filled it (reearth/ygo#251).

type skipDecodeFormat struct {
	name   string
	encode func(*Doc, StateVector) []byte
	merge  func(...[]byte) ([]byte, error)
	apply  func(*Doc, []byte, any) error
}

var skipDecodeFormats = []skipDecodeFormat{
	{"V1", EncodeStateAsUpdateV1, MergeUpdatesV1, ApplyUpdateV1},
	{"V2", EncodeStateAsUpdateV2, MergeUpdatesV2, ApplyUpdateV2},
}

// skipDecodeSteps records the incremental update of each of three sequential
// transactions of one client.
func skipDecodeSteps(t *testing.T, f skipDecodeFormat, edit func(src *Doc, i int)) [][]byte {
	t.Helper()
	src := New(WithClientID(7))
	ups := make([][]byte, 0, 3)
	for i := range 3 {
		before := src.StateVector()
		edit(src, i)
		ups = append(ups, f.encode(src, before))
	}
	return ups
}

func skipDecodeText(d *Doc) string { return d.GetText("t").ToString() }

// skipDecodeTextSteps types "ab", then "cd" after it, then "X" at the start.
// "X" has no origin, only a right origin in "ab", so after a skip over "cd"
// nothing but the skip says it must wait.
func skipDecodeTextSteps(t *testing.T, f skipDecodeFormat) [][]byte {
	t.Helper()
	edits := []struct {
		at   int
		text string
	}{{0, "ab"}, {2, "cd"}, {0, "X"}}
	return skipDecodeSteps(t, f, func(src *Doc, i int) {
		text := src.GetText("t")
		src.Transact(func(txn *Transaction) { text.Insert(txn, edits[i].at, edits[i].text, nil) })
	})
}

func skipDecodeKeys(d *Doc) string {
	keys := d.GetMap("m").Keys()
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func skipDecodeApply(t *testing.T, f skipDecodeFormat, updates ...[]byte) *Doc {
	t.Helper()
	d := New(WithClientID(99))
	for _, u := range updates {
		require.NoError(t, f.apply(d, u, nil))
	}
	return d
}

func TestUnit_SkipDecode_GappedMergeThenFiller(t *testing.T) {
	for _, f := range skipDecodeFormats {
		t.Run(f.name+"/text", func(t *testing.T) {
			ups := skipDecodeTextSteps(t, f)
			gapped, err := f.merge(ups[0], ups[2])
			require.NoError(t, err)

			d := skipDecodeApply(t, f, gapped)
			require.Equal(t, "ab", skipDecodeText(d), "the struct after the skip must wait for the gap")
			require.Equal(t, uint64(2), d.StateVector().Clock(7), "the client's clock must not move past the gap")

			require.Equal(t, "Xabcd", skipDecodeText(skipDecodeApply(t, f, gapped, ups[1])), "gapped merge, then filler")
			require.Equal(t, "Xabcd", skipDecodeText(skipDecodeApply(t, f, ups[1], gapped)), "filler, then gapped merge")
		})
		t.Run(f.name+"/map", func(t *testing.T) {
			keys := []string{"a", "b", "c"}
			ups := skipDecodeSteps(t, f, func(src *Doc, i int) {
				m := src.GetMap("m")
				src.Transact(func(txn *Transaction) { m.Set(txn, keys[i], "1") })
			})
			gapped, err := f.merge(ups[0], ups[2])
			require.NoError(t, err)
			require.Equal(t, "a", skipDecodeKeys(skipDecodeApply(t, f, gapped)))
			require.Equal(t, "a,b,c", skipDecodeKeys(skipDecodeApply(t, f, gapped, ups[1])), "gapped merge, then filler")
			require.Equal(t, "a,b,c", skipDecodeKeys(skipDecodeApply(t, f, ups[1], gapped)), "filler, then gapped merge")
		})
	}
}

// From v1.55.0 the encoder writes parked structs too (reearth/ygo#257): a
// document holding a struct parked behind a skip encodes it behind the same
// skip, a reader parks it again, and the filler still completes the document.
// (On the fork's decoder-only patch, fc0ddac, this test asserted the opposite:
// that encoder still left parked structs out.)
func TestUnit_SkipDecode_EncoderKeepsParkedStructs(t *testing.T) {
	for _, f := range skipDecodeFormats {
		t.Run(f.name, func(t *testing.T) {
			ups := skipDecodeTextSteps(t, f)
			gapped, err := f.merge(ups[0], ups[2])
			require.NoError(t, err)

			d := skipDecodeApply(t, f, gapped)
			require.NotNil(t, d.store.pending, "the struct after the skip must be parked")
			stored := f.encode(d, nil)

			reread := skipDecodeApply(t, f, stored)
			require.Equal(t, "ab", skipDecodeText(reread))
			require.NotNil(t, reread.store.pending, "the stored document must carry the parked struct")
			require.Equal(t, "Xabcd", skipDecodeText(skipDecodeApply(t, f, stored, ups[1])),
				"the filler must complete what was stored")
		})
	}
}
