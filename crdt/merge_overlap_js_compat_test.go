package crdt_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// Yjs mergeUpdatesV2 decides between structs of one client that overlap
// across updates by the order it reads them, not by their length: the struct
// it takes first is kept whole, with every struct that follows it without a
// gap in the same update, and an overlapping struct of another update is cut
// at their end or dropped. These tests merge the same updates with ygo and
// with Yjs and require the same bytes.

// typedAt returns, from clock from on, what client wrote when it typed each
// string at the start of text "t", one transaction per string. Each string
// is a struct of its own: the first has the root as its parent and every
// later one the previous string's first character as its right origin.
func typedAt(t *testing.T, client crdt.ClientID, from uint64, words ...string) []byte {
	t.Helper()
	d := crdt.New(crdt.WithClientID(client))
	defer d.Destroy()
	txt := d.GetText("t")
	for _, w := range words {
		d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, w, nil) })
	}
	return crdt.EncodeStateAsUpdateV2(d, crdt.StateVector{client: from})
}

// typedAfterHigherClient is client 1's "Hi" at clock 0 in an update that
// first carries client 2's "zz", the order Yjs writes clients in.
func typedAfterHigherClient(t *testing.T) []byte {
	t.Helper()
	d1 := crdt.New(crdt.WithClientID(1))
	defer d1.Destroy()
	t1 := d1.GetText("t")
	d1.Transact(func(tx *crdt.Transaction) { t1.Insert(tx, 0, "Hi", nil) })
	d2 := crdt.New(crdt.WithClientID(2))
	defer d2.Destroy()
	require.NoError(t, crdt.ApplyUpdateV2(d2, crdt.EncodeStateAsUpdateV2(d1, nil), nil))
	t2 := d2.GetText("t")
	d2.Transact(func(tx *crdt.Transaction) { t2.Insert(tx, 0, "zz", nil) })
	return crdt.EncodeStateAsUpdateV2(d2, nil)
}

type overlapMergeCase struct {
	Name    string   `json:"name"`
	Updates [][]byte `json:"updates"`
	// V1 also compares the V1 merge. ygo's V1 encoder writes clients in
	// ascending order and Yjs in descending order, so only single-client
	// cases can match byte for byte.
	V1       bool     `json:"v1"`
	UpdateV1 [][]byte `json:"updatesV1,omitempty"`
	Merged   []byte   `json:"merged"`
	MergedV1 []byte   `json:"mergedV1,omitempty"`
}

func overlapMergeCases(t *testing.T) []overlapMergeCase {
	t.Helper()
	hello := typedAt(t, 1, 0, "Hello")                     // [0,5)
	hi := typedAt(t, 1, 0, "Hi")                           // [0,2)
	inside := typedAt(t, 1, 3, "abc", "uvwxyz")            // [3,9)
	helloRun := typedAt(t, 1, 0, "Hello", "abc")           // [0,5) [5,8)
	across := typedAt(t, 1, 3, "xyz", "1234567")           // [3,10)
	contained := typedAt(t, 1, 1, "a", "bc")               // [1,3)
	ten := typedAt(t, 1, 0, "0123456789")                  // [0,10)
	skipped := typedAt(t, 1, 5, "abcde", "fgh", "ijklmno") // [5,8) [8,15)
	wide := typedAt(t, 1, 6, "pqrstu", "ABCDEFGHIJKLMN")   // [6,20)
	short := typedAt(t, 1, 0, "abc")                       // [0,3)
	later := typedAt(t, 1, 5, "abcde", "xyz")              // [5,8)
	higher := typedAfterHigherClient(t)                    // 2:[0,2), then 1:[0,2)
	// [2,5) [12,15), with a gap between them.
	gapped, err := crdt.MergeUpdatesV2(typedAt(t, 1, 2, "ab", "cde"), typedAt(t, 1, 12, "0123456789ab", "xyz"))
	require.NoError(t, err)
	filler := typedAt(t, 1, 10, "0123456789", "klm") // [10,13)

	single := func(name string, updates ...[]byte) overlapMergeCase {
		return overlapMergeCase{Name: name, Updates: updates, V1: true}
	}
	cases := []overlapMergeCase{
		single("same clock, longer first", hello, hi),
		single("same clock, shorter first", hi, hello),
		single("overlap from inside a struct, that struct first", hello, inside),
		single("overlap from inside a struct, the overlapping one first", inside, hello),
		single("overlap across a run's struct boundary, run first", helloRun, across),
		single("overlap across a run's struct boundary, run last", across, helloRun),
		single("contained, outer first", hello, contained),
		single("contained, inner first", contained, hello),
		single("a struct after dropped ones is cut, not re-sorted", ten, skipped, wide),
		single("a struct after dropped ones is cut, reversed", wide, skipped, ten),
		single("a gap after dropped structs is filled from another update first", ten, gapped, filler),
		single("gap", short, later),
		single("gap, reversed", later, short),
		single("same clock, three updates", hi, hello, ten),
		{Name: "same clock after a higher client, higher-client update last", Updates: [][]byte{hello, higher}},
		{Name: "same clock after a higher client, higher-client update first", Updates: [][]byte{higher, hello}},
	}
	for i := range cases {
		c := &cases[i]
		merged, err := crdt.MergeUpdatesV2(c.Updates...)
		require.NoError(t, err, c.Name)
		c.Merged = merged
		if !c.V1 {
			continue
		}
		for _, u := range c.Updates {
			v1, err := crdt.UpdateV2ToV1(u)
			require.NoError(t, err, c.Name)
			c.UpdateV1 = append(c.UpdateV1, v1)
		}
		mergedV1, err := crdt.MergeUpdatesV1(c.UpdateV1...)
		require.NoError(t, err, c.Name)
		c.MergedV1 = mergedV1
	}
	return cases
}

// runYjs runs script with node, passing yjs's path and a file holding input
// as JSON, and decodes the JSON the script prints.
func runYjs(t *testing.T, script string, input, output any) {
	t.Helper()
	nodePath, _ := requireConformance(t, "yjs")
	yjsPath, err := filepath.Abs(filepath.Join("..", "testutil", "node_modules", "yjs"))
	require.NoError(t, err)
	dir := t.TempDir()
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	inputPath := filepath.Join(dir, "input.json")
	require.NoError(t, os.WriteFile(inputPath, raw, 0o644))
	scriptPath := filepath.Join(dir, "script.js")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o644))
	out, err := exec.Command(nodePath, scriptPath, yjsPath, inputPath).Output()
	if ee, ok := err.(*exec.ExitError); ok {
		t.Logf("node stderr:\n%s", ee.Stderr)
	}
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(out, output), "node output: %s", out)
}

func TestCompat_MergeUpdates_OverlappingStructsMatchYjs(t *testing.T) {
	cases := overlapMergeCases(t)
	const script = `
const Y = require(process.argv[2]);
const cases = JSON.parse(require('fs').readFileSync(process.argv[3], 'utf8'));
const b = s => new Uint8Array(Buffer.from(s, 'base64'));
const s = u => Buffer.from(u).toString('base64');
const text = (u, v1) => {
  const d = new Y.Doc();
  (v1 ? Y.applyUpdate : Y.applyUpdateV2)(d, u);
  return d.getText('t').toString();
};
const out = cases.map(c => {
  const merged = Y.mergeUpdatesV2(c.updates.map(b));
  const r = { merged: s(merged), text: text(merged, false) };
  if (c.v1) {
    const mergedV1 = Y.mergeUpdates(c.updatesV1.map(b));
    r.mergedV1 = s(mergedV1);
  }
  return r;
});
process.stdout.write(JSON.stringify(out));
`
	var want []struct {
		Merged   []byte `json:"merged"`
		Text     string `json:"text"`
		MergedV1 []byte `json:"mergedV1"`
	}
	runYjs(t, script, cases, &want)
	require.Len(t, want, len(cases))
	for i, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			got := crdt.New()
			defer got.Destroy()
			require.NoError(t, crdt.ApplyUpdateV2(got, c.Merged, nil))
			require.Equal(t, want[i].Text, got.GetText("t").ToString(), "ygo's merge reads back differently from Yjs's")
			require.Equal(t, want[i].Merged, c.Merged, "ygo's MergeUpdatesV2 differs from Yjs mergeUpdatesV2")
			if c.V1 {
				require.Equal(t, want[i].MergedV1, c.MergedV1, "ygo's MergeUpdatesV1 differs from Yjs mergeUpdates")
			}
		})
	}
}

// parkedAt is a string struct of client 1 at clock, after client 777's first
// character, which the documents below never receive, so they park it.
func parkedAt(t *testing.T, clock uint64, s string) []byte {
	t.Helper()
	other := crdt.New(crdt.WithClientID(777))
	defer other.Destroy()
	otherText := other.GetText("t")
	other.Transact(func(tx *crdt.Transaction) { otherText.Insert(tx, 0, "x", nil) })
	forger := crdt.New(crdt.WithClientID(1))
	defer forger.Destroy()
	if clock > 0 {
		elsewhere := forger.GetText("elsewhere")
		forger.Transact(func(tx *crdt.Transaction) { elsewhere.Insert(tx, 0, strings.Repeat("-", int(clock)), nil) })
	}
	require.NoError(t, crdt.ApplyUpdateV2(forger, crdt.EncodeStateAsUpdateV2(other, nil), nil))
	forged := forger.GetText("t")
	forger.Transact(func(tx *crdt.Transaction) { forged.Insert(tx, 1, s, nil) })
	return crdt.EncodeStateAsUpdateV2(forger, crdt.StateVector{1: clock, 777: 1})
}

// A document's encoding merges the structs it parks into its integrated ones.
// A parked struct that overlaps integrated structs of its client must not
// replace them. Every integrated character reads back from the encoding, in
// order; the only characters it can add are the parked struct's own, from
// where the client's integrated clocks end, which is what Yjs's
// encodeStateAsUpdateV2 of the same document reads back as too.
func TestCompat_EncodeStateAsUpdate_IntegratedStructsWinOverParked(t *testing.T) {
	type scenario struct {
		Name    string   `json:"name"`
		Updates [][]byte `json:"updates"`
		// Covered says the parked struct lies within the client's integrated
		// clocks, so nothing of it reads back.
		Covered bool `json:"-"`
	}
	helloWorld := typedAt(t, 1, 0, "Hello", "world ") // [0,5) [5,11): "world Hello"
	hello := typedAt(t, 1, 0, "Hello")
	editor := crdt.New(crdt.WithClientID(1))
	defer editor.Destroy()
	require.NoError(t, crdt.ApplyUpdateV2(editor, hello, nil))
	before := editor.StateVector()
	text := editor.GetText("t")
	editor.Transact(func(tx *crdt.Transaction) { text.Insert(tx, 5, "abc", nil) })
	editor.Transact(func(tx *crdt.Transaction) { text.Insert(tx, 0, strings.Repeat("h", 20), nil) })
	honest := crdt.EncodeStateAsUpdateV2(editor, before) // client 1's next write: [5,8) [8,28)

	// Parked structs hold only "z", which no integrated text holds.
	scenarios := []scenario{
		{Name: "parked inside the client's next write, which then lands", Covered: true,
			Updates: [][]byte{hello, parkedAt(t, 7, strings.Repeat("z", 10)), honest}},
		{Name: "parked at the clock of the client's next write and longer, which then lands",
			Updates: [][]byte{hello, parkedAt(t, 5, strings.Repeat("z", 30)), honest}},
		{Name: "parked at an integrated struct's clock and longer",
			Updates: [][]byte{helloWorld, parkedAt(t, 0, strings.Repeat("z", 14))}},
		{Name: "parked across two integrated structs",
			Updates: [][]byte{helloWorld, parkedAt(t, 3, strings.Repeat("z", 10))}},
	}
	type result struct {
		Text   string `json:"text"`
		Parked bool   `json:"parked"`
		Stored []byte `json:"stored"`
		Read   string `json:"read"`
	}
	const script = `
const Y = require(process.argv[2]);
const scenarios = JSON.parse(require('fs').readFileSync(process.argv[3], 'utf8'));
const out = scenarios.map(sc => {
  const d = new Y.Doc();
  for (const u of sc.updates) Y.applyUpdateV2(d, new Uint8Array(Buffer.from(u, 'base64')));
  const stored = Y.encodeStateAsUpdateV2(d);
  const r = new Y.Doc();
  Y.applyUpdateV2(r, stored);
  return {
    text: d.getText('t').toString(),
    parked: d.store.pendingStructs !== null,
    stored: Buffer.from(stored).toString('base64'),
    read: r.getText('t').toString(),
  };
});
process.stdout.write(JSON.stringify(out));
`
	var yjs []result
	runYjs(t, script, scenarios, &yjs)
	require.Len(t, yjs, len(scenarios))
	for i, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			doc := crdt.New()
			defer doc.Destroy()
			for _, u := range sc.Updates {
				require.NoError(t, crdt.ApplyUpdateV2(doc, u, nil))
			}
			require.True(t, yjs[i].Parked, "Yjs does not park the forged struct; the scenario no longer tests a merge with parked structs")
			require.Equal(t, 1, doc.PendingStats().Items, "ygo does not park the forged struct")
			integrated := doc.GetText("t").ToString()
			require.Equal(t, yjs[i].Text, integrated)

			stored := crdt.EncodeStateAsUpdateV2(doc, nil)
			readBack := crdt.New()
			defer readBack.Destroy()
			require.NoError(t, crdt.ApplyUpdateV2(readBack, stored, nil))
			read := readBack.GetText("t").ToString()
			require.Equal(t, yjs[i].Read, read, "ygo's encoding reads back differently from Yjs's")
			require.Equal(t, integrated, strings.ReplaceAll(read, "z", ""), "the encoding replaced integrated characters with a parked struct")
			if sc.Covered {
				require.Equal(t, integrated, read)
				require.Equal(t, doc.StateVector(), readBack.StateVector())
			} else {
				require.Contains(t, read, "z", "the parked struct no longer reaches past the integrated clocks; revisit the case")
			}
		})
	}
}
