package crdt

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// YText.Delete's format-marker clean-up used to answer each closer by walking
// Left to the nearest live same-key marker. After a deletion that leaves many
// markers behind, that walk crossed every marker the clean-up had just
// tombstoned, so deleting a whole text whose formatting changes on every
// character was quadratic. These tests pin the clean-up's result to the old
// walk and bound the time of a delete over more than 100,000 items.

// leftWalkCleanup is cleanupDanglingFormatsInRegion as it was before
// nearestLiveFormats, kept verbatim as the oracle for the closer lookup.
func leftWalkCleanup(txt *YText, txn *Transaction, startAnchor *Item) {
	var node *Item
	if startAnchor == nil {
		node = txt.firstLiveFromStart()
	} else {
		node = startAnchor.Right
	}
	seenLivePast := false
	for node != nil {
		next := node.Right
		if node.Deleted {
			node = next
			continue
		}
		if cf, ok := node.Content.(*ContentFormat); ok {
			if cf.Val == nil {
				hasLiveOpener := false
				for p := node.Left; p != nil; p = p.Left {
					if p.Deleted {
						continue
					}
					pcf, isFmt := p.Content.(*ContentFormat)
					if !isFmt {
						continue
					}
					if pcf.Key != cf.Key {
						continue
					}
					if pcf.Val != nil {
						hasLiveOpener = true
					}
					break
				}
				if !hasLiveOpener {
					node.delete(txn)
				}
			} else {
				hasLiveInScope := false
				for n := node.Right; n != nil; n = n.Right {
					if n.Deleted {
						continue
					}
					if ncf, isFmt := n.Content.(*ContentFormat); isFmt {
						if ncf.Key == cf.Key {
							break
						}
						continue
					}
					if n.Content.IsCountable() {
						hasLiveInScope = true
						break
					}
				}
				if !hasLiveInScope {
					node.delete(txn)
				}
			}
			node = next
			continue
		}
		if node.Content.IsCountable() {
			if seenLivePast {
				break
			}
			seenLivePast = true
		}
		node = next
	}
}

// leftWalkDelete is YText.Delete running leftWalkCleanup.
func leftWalkDelete(txt *YText, txn *Transaction, index, length int) {
	var startAnchor *Item
	if index > 0 {
		startAnchor, _ = txt.leftNeighbourAt(index)
	}
	deleteRange(&txt.abstractType, txn, index, length)
	if !txt.hasFormatting {
		return
	}
	leftWalkCleanup(txt, txn, startAnchor)
}

var formatCleanupAttrs = []Attributes{
	nil,
	{"bold": true},
	{"italic": true},
	{"bold": true, "italic": true},
	{"link": "https://example.com/a"},
	{"link": "https://example.com/b", "color": "#f00"},
	{"bold": nil},
	{"italic": nil, "color": nil},
}

func randomAttrs(r *rand.Rand) Attributes {
	return formatCleanupAttrs[r.Intn(len(formatCleanupAttrs))]
}

// randomTextEdit inserts, embeds, formats or deletes at a random position.
func randomTextEdit(r *rand.Rand, txn *Transaction, text *YText, deletes bool) {
	n := text.Len()
	switch op := r.Intn(10); {
	case op < 5 || n == 0:
		text.Insert(txn, r.Intn(n+1), string(rune('a'+r.Intn(26))), randomAttrs(r))
	case op < 6:
		text.InsertEmbed(txn, r.Intn(n+1), map[string]any{"image": fmt.Sprintf("i%d", r.Intn(9))}, randomAttrs(r))
	case op < 8 || !deletes:
		i := r.Intn(n)
		attrs := randomAttrs(r)
		if len(attrs) == 0 {
			attrs = Attributes{"bold": true}
		}
		text.Format(txn, i, 1+r.Intn(n-i), attrs)
	default:
		i := r.Intn(n)
		text.Delete(txn, i, 1+r.Intn(min(n-i, 8)))
	}
}

// formatCleanupDocs builds the documents the equivalence test deletes from.
func formatCleanupDocs(r *rand.Rand) map[string][]byte {
	docs := make(map[string][]byte)

	// One session typing at the start, formatting alternating per
	// character, with embeds: the shape that made the walk quadratic.
	typed := newTestDoc(3_000_000_000)
	text := typed.GetText("t")
	for i := range 400 {
		typed.Transact(func(txn *Transaction) {
			switch {
			case i%37 == 36:
				text.InsertEmbed(txn, 0, map[string]any{"image": "x"}, Attributes{"link": "https://example.com"})
			case i%2 == 0:
				text.Insert(txn, 0, "x", Attributes{"bold": true})
			default:
				text.Insert(txn, 0, "y", nil)
			}
		})
	}
	docs["type-ahead alternating bold"] = EncodeStateAsUpdateV2(typed, nil)

	// Several keys over overlapping ranges, including removals, with
	// earlier deletions leaving tombstoned text and markers behind.
	for _, deletes := range []bool{false, true} {
		doc := newTestDoc(1001)
		text := doc.GetText("t")
		for range 300 {
			doc.Transact(func(txn *Transaction) { randomTextEdit(r, txn, text, deletes) })
		}
		docs[fmt.Sprintf("overlapping keys, earlier deletes %v", deletes)] = EncodeStateAsUpdateV2(doc, nil)
	}

	// Three sessions editing concurrently from a shared base, merged, so
	// remote clean-up has run and markers from several clients interleave.
	base := newTestDoc(7)
	baseText := base.GetText("t")
	base.Transact(func(txn *Transaction) { baseText.Insert(txn, 0, "Hello world", Attributes{"bold": true}) })
	baseUpdate := EncodeStateAsUpdateV2(base, nil)
	clients := []ClientID{3_000_000_000, 500, 2_000_000_000}
	updates := make([][]byte, 0, len(clients))
	for _, client := range clients {
		doc := New(WithClientID(client))
		if err := ApplyUpdateV2(doc, baseUpdate, nil); err != nil {
			panic(err)
		}
		text := doc.GetText("t")
		for range 120 {
			doc.Transact(func(txn *Transaction) { randomTextEdit(r, txn, text, true) })
		}
		updates = append(updates, EncodeStateAsUpdateV2(doc, nil))
	}
	merged := newTestDoc(9)
	for _, u := range updates {
		if err := ApplyUpdateV2(merged, u, nil); err != nil {
			panic(err)
		}
	}
	docs["three sessions merged"] = EncodeStateAsUpdateV2(merged, nil)

	plain := newTestDoc(42)
	plainText := plain.GetText("t")
	for range 200 {
		plain.Transact(func(txn *Transaction) { plainText.Insert(txn, 0, "p", nil) })
	}
	docs["plain text"] = EncodeStateAsUpdateV2(plain, nil)
	return docs
}

func requireSameTextState(t *testing.T, a, b *Doc, ta, tb *YText, msg string) {
	t.Helper()
	require.True(t, bytes.Equal(EncodeStateAsUpdateV2(a, nil), EncodeStateAsUpdateV2(b, nil)), "%s: update-v2 differs", msg)
	require.True(t, bytes.Equal(EncodeStateAsUpdateV1(a, nil), EncodeStateAsUpdateV1(b, nil)), "%s: update-v1 differs", msg)
	require.Equal(t, tb.ToDelta(), ta.ToDelta(), "%s: delta differs", msg)
	ia, ib := ta.start, tb.start
	for ia != nil && ib != nil {
		require.Equal(t, ib.ID, ia.ID, msg)
		require.Equal(t, ib.Deleted, ia.Deleted, "%s: item %v", msg, ia.ID)
		ia, ib = ia.Right, ib.Right
	}
	require.True(t, ia == nil && ib == nil, "%s: item lists differ in length", msg)
}

// The clean-up tombstones exactly the markers the Left walk did, so every
// encoding of the document is unchanged, across formatting keys and values,
// removals, embeds, earlier tombstones, several clients, single and several
// deletes per transaction, and deletes from the start, middle and end.
func TestUnit_YText_DeleteFormatCleanupMatchesLeftWalk(t *testing.T) {
	seeds := raceSeeds(30)
	for seed := range seeds {
		r := rand.New(rand.NewSource(int64(seed)))
		for name, update := range formatCleanupDocs(r) {
			fixed, oracle := newTestDoc(77), newTestDoc(77)
			require.NoError(t, ApplyUpdateV2(fixed, update, nil))
			require.NoError(t, ApplyUpdateV2(oracle, update, nil))
			tf, to := fixed.GetText("t"), oracle.GetText("t")
			requireSameTextState(t, fixed, oracle, tf, to, name+" loaded")
			for step := range 12 {
				n := tf.Len()
				if n == 0 {
					break
				}
				type del struct{ index, length int }
				var dels []del
				switch kind := r.Intn(6); {
				case kind == 0 || step == 11:
					dels = []del{{0, n}}
				case kind == 1:
					dels = []del{{0, 1 + r.Intn(n)}}
				case kind == 2:
					i := r.Intn(n)
					dels = []del{{i, n - i}}
				case kind == 3:
					// Several deletes in one transaction.
					left := n
					for range 1 + r.Intn(4) {
						if left == 0 {
							break
						}
						i := r.Intn(left)
						l := 1 + r.Intn(min(left-i, 20))
						dels = append(dels, del{i, l})
						left -= l
					}
				default:
					i := r.Intn(n)
					dels = []del{{i, 1 + r.Intn(n-i)}}
				}
				fixed.Transact(func(txn *Transaction) {
					for _, d := range dels {
						tf.Delete(txn, d.index, d.length)
					}
				})
				oracle.Transact(func(txn *Transaction) {
					for _, d := range dels {
						leftWalkDelete(to, txn, d.index, d.length)
					}
				})
				msg := fmt.Sprintf("seed %d, %s, step %d, deletes %v", seed, name, step, dels)
				requireSameTextState(t, fixed, oracle, tf, to, msg)

				// Edit both the same way so the next delete meets new markers.
				er := rand.New(rand.NewSource(int64(seed*100 + step)))
				or := rand.New(rand.NewSource(int64(seed*100 + step)))
				for range r.Intn(6) {
					fixed.Transact(func(txn *Transaction) { randomTextEdit(er, txn, tf, false) })
					oracle.Transact(func(txn *Transaction) { randomTextEdit(or, txn, to, false) })
				}
				requireSameTextState(t, fixed, oracle, tf, to, msg+" after edits")
			}
		}
	}
}

// typeAheadFormatted is one session typing chars characters at the start of
// the text, alternating bold and plain, with an embed every 1,000: about two
// items per character, every second one a format marker.
func typeAheadFormatted(chars int) []byte {
	doc := newTestDoc(3_000_000_000)
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		for i := range chars {
			switch {
			case i%1000 == 999:
				text.InsertEmbed(txn, 0, map[string]any{"image": "x"}, nil)
			case i%2 == 0:
				text.Insert(txn, 0, "x", Attributes{"bold": true})
			default:
				text.Insert(txn, 0, "y", nil)
			}
		}
	})
	return EncodeStateAsUpdateV2(doc, nil)
}

// Deleting the whole of a 110,000-character text with about 220,000 items
// takes well under a second; with the Left walk it took over a minute.
func TestUnit_YText_DeleteWholeFormattedTextIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 220,000-item text")
	}
	const chars = 110_000
	update := typeAheadFormatted(chars)
	doc := newTestDoc(77)
	require.NoError(t, ApplyUpdateV2(doc, update, nil))
	text := doc.GetText("t")
	items := 0
	for it := text.start; it != nil; it = it.Right {
		items++
	}
	require.Greater(t, items, 200_000)
	length := text.Len()

	start := time.Now()
	doc.Transact(func(txn *Transaction) { text.Delete(txn, 0, length) })
	elapsed := time.Since(start)
	t.Logf("deleted %d characters over %d items in %v", length, items, elapsed)

	require.Zero(t, text.Len())
	require.Empty(t, text.ToDelta())
	live := 0
	for it := text.start; it != nil; it = it.Right {
		if !it.Deleted {
			live++
		}
	}
	require.Zero(t, live, "every format marker is redundant once the text is gone")
	// The Left walk took over 60 s here; the bound leaves room for -race and
	// a loaded machine.
	require.Less(t, elapsed, 20*time.Second)
}

func BenchmarkYTextDeleteWholeFormattedText(b *testing.B) {
	for _, chars := range []int{10_000, 100_000} {
		update := typeAheadFormatted(chars)
		b.Run(fmt.Sprintf("chars=%d", chars), func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				doc := newTestDoc(77)
				if err := ApplyUpdateV2(doc, update, nil); err != nil {
					b.Fatal(err)
				}
				text := doc.GetText("t")
				length := text.Len()
				b.StartTimer()
				doc.Transact(func(txn *Transaction) { text.Delete(txn, 0, length) })
			}
		})
	}
}
