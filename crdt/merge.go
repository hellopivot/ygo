package crdt

import (
	"container/heap"
	"sort"

	"github.com/reearth/ygo/encoding"
)

// This file implements struct-level merge / diff / state-vector extraction for
// V1 updates (and, in merge_v2.go, the V2 columnar format). The previous
// MergeUpdatesV1 / DiffUpdateV1 applied updates to a temporary Doc and
// re-encoded its INTEGRATED state, which silently dropped any struct whose
// dependency was missing (it parked in store.pending and was never emitted) —
// corrupting the common partial-diff case (#125). These functions instead
// operate at the struct level, like Yjs mergeUpdates / diffUpdate: decode every
// struct WITHOUT integrating, merge per client (dedup/slice overlaps, preserve
// clock gaps as skip structs), and re-encode — so non-integrable structs are
// preserved verbatim for a downstream consumer that already has their deps.

// decodeStructsV1 decodes every struct in a V1 update into per-client,
// clock-ordered slices (without integrating) plus the update's delete set.
// scratch is a throwaway Doc used only to resolve parent type names during
// per-struct decode; it is never mutated structurally.
func decodeStructsV1(scratch *Doc, update []byte) (map[ClientID][]*Item, DeleteSet, error) {
	dec := encoding.NewDecoder(update)
	numClients, err := dec.ReadVarUint()
	if err != nil {
		return nil, DeleteSet{}, wrapUpdateErr(err)
	}
	if numClients > maxV2Items {
		return nil, DeleteSet{}, ErrInvalidUpdate
	}
	// Don't pre-size by numClients: it's attacker-controlled (a tiny update can
	// claim up to maxV2Items clients), so a size hint would amplify allocation.
	// The map grows as real groups are read.
	out := make(map[ClientID][]*Item)
	total := uint64(0)
	for i := uint64(0); i < numClients; i++ {
		numStructs, err := dec.ReadVarUint()
		if err != nil {
			return nil, DeleteSet{}, wrapUpdateErr(err)
		}
		total += numStructs
		if total > maxV2Items {
			return nil, DeleteSet{}, ErrInvalidUpdate
		}
		clientU, err := dec.ReadVarUint()
		if err != nil {
			return nil, DeleteSet{}, wrapUpdateErr(err)
		}
		client := ClientID(clientU)
		clock, err := dec.ReadVarUint()
		if err != nil {
			return nil, DeleteSet{}, wrapUpdateErr(err)
		}
		// A later group for the client replaces earlier ones, as in apply (yjs parity).
		var structs []*Item
		for j := uint64(0); j < numStructs; j++ {
			item, err := decodeItem(dec, scratch, client, clock)
			if err != nil {
				return nil, DeleteSet{}, wrapUpdateErr(err)
			}
			clock += uint64(item.Content.Len())
			// Skip structs are clock-range placeholders, not content. Drop them
			// from the struct list — advancing clock above leaves a gap that the
			// encoder re-emits as a skip (Yjs filterSkips). Carrying them would
			// later hand a contentSkip to encodeItem, which can't encode it.
			if _, isSkip := item.Content.(*contentSkip); isSkip {
				continue
			}
			structs = append(structs, item)
		}
		out[client] = structs
	}
	ds, err := decodeDeleteSet(dec)
	if err != nil {
		return nil, DeleteSet{}, wrapUpdateErr(err)
	}
	return out, ds, nil
}

// sliceItemFront mutates item to drop its first `offset` logical units, so it
// starts `offset` clocks later. Used to cut overlapping struct ranges when
// merging. Content.Splice(offset) mutates the receiver to hold [0,offset) and
// returns the suffix; we keep the suffix.
func sliceItemFront(item *Item, offset int) {
	// Like Yjs sliceStruct, the cut item follows its own client's previous
	// clock, whatever anchors it had: one with only a right origin or a parent
	// by ID (its parent is not resolved yet while merging) must not keep
	// following those alone. A GC placeholder must stay anchorless so it
	// re-encodes as a GC struct. Skip structs never reach here — they're
	// filtered out during decode.
	isItem := !item.isGCOrphan()
	suffix := item.Content.Splice(offset)
	item.ID.Clock += uint64(offset)
	if isItem {
		item.Origin = &ID{Client: item.ID.Client, Clock: item.ID.Clock - 1}
	}
	item.Content = suffix
}

// mergeClientStructs merges the structs of several decoded updates into
// per-client, clock-ascending, non-overlapping slices, deciding between
// overlapping structs of one client the way Yjs mergeUpdatesV2
// (src/utils/updates.js) does. Clock gaps are preserved (the encoder emits
// skip structs for them).
//
// Yjs reads every update as a stream, its clients in the order the update's
// bytes list them and each client's structs by clock, and takes structs by
// client, descending, and clock across the streams. A struct it takes is kept
// whole, and so is every struct that follows it without a gap in the same
// stream. A struct of another stream that ends within what it has taken is
// dropped, and one that reaches past it is cut there. Which of two structs at
// the same clock it takes first depends on the order of the streams, not on
// the structs' lengths: the streams stay in the order Yjs's stable sort left
// them in, where the stream it last took from comes first and the streams it
// has not taken from keep the order of the updates.
//
// So when a document's encoded state is merged with the structs it parks, as
// withParked does, no parked struct replaces an integrated one: each client's
// integrated structs run without a gap from the lowest clock any of the
// updates holds for it, and the state is the first update, while each parked
// struct is an update of its own. That matters because a parked struct can be
// forged for another client's clocks: it must not replace that client's
// integrated characters in the encoding.
//
// Yjs's loop relies on every update listing its clients in descending order,
// as Yjs's encoders write them. A decoded update here keeps no order of
// clients, so each stream lists them in descending order, whatever order the
// update's bytes used. ygo's V2 encoder writes clients in descending order as
// well, but its V1 encoder writes them in ascending order. For ygo's own V1
// updates with more than one client, MergeUpdatesV1 therefore gives what Yjs
// mergeUpdates gives for the same structs listed in descending order, not
// what it gives for the bytes ygo wrote: given clients in ascending order,
// Yjs can take the other of two structs at one clock, and can drop a higher
// client's structs. Merging ygo's V1 update of client 1's "Hello" and client
// 2's "zz" with client 1's "Hi", Yjs mergeUpdates drops "zz", while
// MergeUpdatesV1 keeps "Hello" and "zz", as Yjs does when that update lists
// client 2 first.
//
// Two Yjs details are left out. It merges adjacent GC structs of different
// updates into one, which changes the bytes but not the clocks they cover,
// and its sort orders a GC struct and an item at the same clock
// inconsistently; here they keep the order of the streams.
func mergeClientStructs(updates []map[ClientID][]*Item) map[ClientID][]*Item {
	if len(updates) == 1 {
		return updates[0] // Yjs returns a single update as is
	}
	out := make(map[ClientID][]*Item)
	streams := make(structStreams, 0, len(updates))
	for i, u := range updates {
		s := &structStream{index: i}
		// Clients in descending order, whatever order the bytes used.
		clients := clientsSorted(u)
		for k := len(clients) - 1; k >= 0; k-- {
			s.structs = append(s.structs, u[clients[k]]...)
		}
		if len(s.structs) > 0 {
			streams = append(streams, s)
		}
	}
	heap.Init(&streams)
	end := func(it *Item) uint64 { return it.ID.Clock + uint64(it.Content.Len()) }
	// write is the struct taken last and not yet emitted: Yjs's currWrite.
	var write *Item
	take := func(it *Item) {
		if write != nil {
			out[write.ID.Client] = append(out[write.ID.Client], write)
		}
		write = it
	}
	for taken := 1; len(streams) > 0; taken++ {
		s := streams[0]
		curr := s.curr()
		client := curr.ID.Client
		if write != nil && write.ID.Client == client {
			dropped := false
			for curr != nil && curr.ID.Client == client && end(curr) <= end(write) {
				curr = s.next()
				dropped = true
			}
			switch {
			case curr == nil || curr.ID.Client != client || (dropped && curr.ID.Clock > end(write)):
				// Another stream may hold this client's structs below curr.
				curr = nil
			case curr.ID.Clock < end(write):
				sliceItemFront(curr, int(end(write)-curr.ID.Clock))
			}
		}
		if curr != nil {
			take(curr)
			for next := s.next(); next != nil && next.ID.Client == client && next.ID.Clock == end(write); next = s.next() {
				take(next)
			}
		}
		s.taken = taken
		if s.curr() == nil {
			heap.Pop(&streams)
		} else {
			heap.Fix(&streams, 0)
		}
	}
	take(nil)
	return out
}

// structStream is one update's structs in the order Yjs's LazyStructReader
// yields them.
type structStream struct {
	structs []*Item
	pos     int
	index   int // the update's position among those merged
	taken   int // when mergeClientStructs last took from it; 0 if never
}

func (s *structStream) curr() *Item {
	if s.pos < len(s.structs) {
		return s.structs[s.pos]
	}
	return nil
}

func (s *structStream) next() *Item {
	s.pos++
	return s.curr()
}

// structStreams orders streams as Yjs sorts its struct readers: by client,
// descending, then by clock, with ties left in the order of the previous
// sort, which puts the stream taken from last first.
type structStreams []*structStream

func (h structStreams) Len() int { return len(h) }
func (h structStreams) Less(i, j int) bool {
	a, b := h[i].curr().ID, h[j].curr().ID
	switch {
	case a.Client != b.Client:
		return a.Client > b.Client
	case a.Clock != b.Clock:
		return a.Clock < b.Clock
	case h[i].taken != h[j].taken:
		return h[i].taken > h[j].taken
	}
	return h[i].index < h[j].index
}
func (h structStreams) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *structStreams) Push(x any)   { *h = append(*h, x.(*structStream)) }
func (h *structStreams) Pop() any {
	old := *h
	s := old[len(old)-1]
	*h = old[:len(old)-1]
	return s
}

// encodeStructStoreV1 encodes per-client struct slices (clock-ordered,
// non-overlapping) plus a delete set into a V1 update. Clock gaps within a
// client are filled with skip structs (info tag 10). sv filters out structs
// fully below the receiver's state. store provides origin look-ups for
// encodeItem's GC-placeholder check.
func encodeStructStoreV1(perClient map[ClientID][]*Item, ds DeleteSet, sv StateVector, store *StructStore) []byte {
	clients := make([]ClientID, 0, len(perClient))
	for c, items := range perClient {
		if len(items) > 0 {
			clients = append(clients, c)
		}
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })

	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		// First pass per client: build the emit sequence (skips for gaps +
		// items above sv) so we can write the exact struct count up front.
		type entry struct {
			item   *Item // nil => skip struct
			offset int
			skip   uint64
		}
		type group struct {
			client     ClientID
			startClock uint64
			entries    []entry
		}
		var groups []group
		for _, c := range clients {
			svClock := sv.Clock(c)
			items := perClient[c]
			var ents []entry
			startClock := uint64(0)
			started := false
			expected := uint64(0)
			for _, it := range items {
				end := it.ID.Clock + uint64(it.Content.Len())
				if end <= svClock {
					continue // already known to the receiver
				}
				offset := 0
				clk := it.ID.Clock
				if clk < svClock {
					offset = int(svClock - clk)
					clk = svClock
				}
				if !started {
					startClock = clk
					expected = clk
					started = true
				}
				if clk > expected {
					ents = append(ents, entry{skip: clk - expected})
				}
				ents = append(ents, entry{item: it, offset: offset})
				expected = end
			}
			if started {
				groups = append(groups, group{client: c, startClock: startClock, entries: ents})
			}
		}

		enc.WriteVarUint(uint64(len(groups)))
		for _, g := range groups {
			enc.WriteVarUint(uint64(len(g.entries)))
			enc.WriteVarUint(uint64(g.client))
			enc.WriteVarUint(g.startClock)
			for _, e := range g.entries {
				if e.item == nil {
					enc.WriteUint8(10) // skip struct
					enc.WriteVarUint(e.skip)
					continue
				}
				encodeItem(enc, e.item, e.offset, store)
			}
		}
		encodeDeleteSet(enc, ds)
	})
}

// buildMergeStore decodes every update into one merged set of per-client struct
// slices (overlaps cut as Yjs does, gaps preserved) and the union of their
// delete sets, plus a StructStore holding every struct for origin look-ups
// during encode.
func buildMergeStore(updates [][]byte, decode func(*Doc, []byte) (map[ClientID][]*Item, DeleteSet, error)) (map[ClientID][]*Item, DeleteSet, *StructStore, error) {
	scratch := New()
	decoded := make([]map[ClientID][]*Item, 0, len(updates))
	mergedDS := newDeleteSet()
	for _, u := range updates {
		structs, ds, err := decode(scratch, u)
		if err != nil {
			return nil, DeleteSet{}, nil, err
		}
		decoded = append(decoded, structs)
		mergedDS.Merge(ds)
	}
	perClient := mergeClientStructs(decoded)
	store := &StructStore{clients: make(map[ClientID][]*Item, len(perClient))}
	for c, items := range perClient {
		store.clients[c] = items
	}
	resolveParents(store)
	return perClient, mergedDS, store, nil
}

// resolveParents fills in Parent (and inherited ParentSub) for origin-inferred
// structs whose origin item is present in the merged set. decodeItem leaves
// these nil because the structs are never integrated, but encodeItem treats a
// nil-Parent origin item as a GC placeholder and would clear the dependent's
// origin — breaking e.g. a YMap key's origin chain (#125). Resolving here
// (mirroring decodeItem's origin inheritance, against the full struct set)
// keeps the chain intact. Structs whose origin is NOT present (a diff that
// references prior state) keep a nil Parent and their origin on the wire, so
// the receiver infers the parent once it has the dependency.
func resolveParents(store *StructStore) {
	// Follow each origin chain once. Structs on a chain that reaches no parent
	// are marked so later walks stop there. Origin cycles are caught with
	// Brent's algorithm, which keeps each walk linear in the chain's length.
	var unresolved map[*Item]bool
	var chain []*Item
	for _, items := range store.clients {
		for _, it := range items {
			if it.Parent != nil {
				continue
			}
			ref := parentRef(store, it)
			if ref != nil && ref.Parent != nil {
				inheritParent(it, ref)
				continue
			}
			chain = append(chain[:0], it)
			mark, next := it, 2
			for ref != nil && ref.Parent == nil {
				if ref == mark || unresolved[ref] {
					ref = nil
					break
				}
				chain = append(chain, ref)
				if len(chain) == next {
					mark, next = ref, 2*next
				}
				ref = parentRef(store, ref)
			}
			for i := len(chain) - 1; i >= 0; i-- {
				if ref == nil {
					if unresolved == nil {
						unresolved = make(map[*Item]bool)
					}
					unresolved[chain[i]] = true
					continue
				}
				inheritParent(chain[i], ref)
				ref = chain[i]
			}
		}
	}
}

// parentRef returns the struct whose parent it would inherit.
func parentRef(store *StructStore, it *Item) *Item {
	switch {
	case it.Origin != nil:
		return store.Find(*it.Origin)
	case it.OriginRight != nil:
		return store.Find(*it.OriginRight)
	}
	return nil
}

func inheritParent(it, ref *Item) {
	it.Parent = ref.Parent
	if it.ParentSub == nil {
		it.ParentSub = ref.ParentSub
	}
}

// MergeUpdatesV1 merges several V1 updates into one. Structs are merged at the
// struct level (preserving non-integrable structs and clock gaps) and delete
// sets are unioned — matching Yjs mergeUpdates. (Previously this applied to a
// temp Doc and re-encoded, silently dropping parked structs; #125.)
func MergeUpdatesV1(updates ...[]byte) ([]byte, error) {
	perClient, ds, store, err := buildMergeStore(updates, decodeStructsV1)
	if err != nil {
		return nil, err
	}
	return encodeStructStoreV1(perClient, ds, StateVector{}, store), nil
}

// DiffUpdateV1 returns the portion of update that a peer with state vector sv is
// missing: structs whose clock range extends past sv are kept (sliced at the sv
// boundary), and the full delete set is preserved (deletions are small and
// idempotent — matching Yjs diffUpdate). (Previously this dropped parked
// structs; #125.)
func DiffUpdateV1(update []byte, sv StateVector) ([]byte, error) {
	perClient, ds, store, err := buildMergeStore([][]byte{update}, decodeStructsV1)
	if err != nil {
		return nil, err
	}
	return encodeStructStoreV1(perClient, ds, sv, store), nil
}

// EncodeStateVectorFromUpdate extracts the state vector described by a V1 update
// — the next clock per client (max struct end) — without integrating it. Useful
// for computing what a peer is missing directly from an update. (#57)
func EncodeStateVectorFromUpdate(update []byte) ([]byte, error) {
	perClient, _, _, err := buildMergeStore([][]byte{update}, decodeStructsV1)
	if err != nil {
		return nil, err
	}
	sv := make(StateVector, len(perClient))
	for c, items := range perClient {
		var maxEnd uint64
		for _, it := range items {
			if end := it.ID.Clock + uint64(it.Content.Len()); end > maxEnd {
				maxEnd = end
			}
		}
		sv[c] = maxEnd
	}
	clients := clientsSorted(sv)
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(uint64(len(clients)))
		for _, c := range clients {
			enc.WriteVarUint(uint64(c))
			enc.WriteVarUint(sv[c])
		}
	}), nil
}
