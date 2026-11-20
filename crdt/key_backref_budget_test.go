package crdt

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// craftKeyBackrefV2 builds an update-v2 of n structs from client 1 under root
// "t", each one chained to the one before by its left origin. Every struct
// carries the same keyLen-byte key: format structs (null value) as their
// format key, XmlElements (xml) as their node name. The first struct writes
// the key in full and every later one back-references it, which no Yjs, yrs
// or ygo encoder does (writeKey). Every encoder writes each key in full, so
// without a bound, re-encoding the decoded document stores n*keyLen bytes.
func craftKeyBackrefV2(n, keyLen int, xml bool) []byte {
	return craftKeyedV2(n, keyLen, xml, true)
}

// craftKeyedV2 is craftKeyBackrefV2, except that with backref false every
// struct writes its key in full, as every encoder does.
func craftKeyedV2(n, keyLen int, xml, backref bool) []byte {
	enc := newV2Encoder()
	key := strings.Repeat("k", keyLen)
	client := ClientID(1)
	tag := wireFormat
	if xml {
		tag = wireType
	}
	enc.restEnc.WriteVarUint(1) // one client
	enc.restEnc.WriteVarUint(uint64(n))
	enc.writeClient(client)
	enc.restEnc.WriteVarUint(0) // first clock
	for i := 0; i < n; i++ {
		if i == 0 {
			enc.writeInfo(tag)
			enc.writeParentInfo(true)
			enc.writeString("t")
		} else {
			enc.writeInfo(tag | flagHasOrigin)
			enc.writeLeftID(ID{Client: client, Clock: uint64(i - 1)})
		}
		if xml {
			enc.writeTypeRef(3) // YXmlElement: its node name is a key
		}
		if i == 0 || !backref {
			enc.writeKey(key)
		} else {
			enc.keyClockEnc.Write(0) // the key the first struct wrote
		}
		if !xml {
			enc.restEnc.WriteAny(nil)
		}
	}
	encodeDeleteSetV2(enc, DeleteSet{})
	return enc.toBytes()
}

// keyBackrefs scans an update-v2's keyClock column the way readKey resolves
// it: a value below the number of keys written in full so far is a
// back-reference, and any other value reads a fresh key. It reads only that
// column, so it counts what any decoder would resolve, with or without a
// budget.
func keyBackrefs(t *testing.T, update []byte) (backrefs, fresh int) {
	t.Helper()
	dec, err := newV2Decoder(update)
	if err != nil {
		t.Fatalf("newV2Decoder: %v", err)
	}
	for {
		v, err := dec.keyClockDec.Read()
		if err != nil {
			return backrefs, fresh
		}
		switch {
		case v < 0:
			t.Fatalf("keyClock %d is negative", v)
		case v < int64(fresh):
			backrefs++
		default:
			fresh++
		}
	}
}

// requireRefusedExpansion applies update to a fresh document and requires the
// key back-reference budget to refuse it before the document stores the
// expansion: at most 2x the update, the structs integrated before the refusal
// plus their keys written in full.
func requireRefusedExpansion(t *testing.T, update []byte, what string) {
	t.Helper()
	doc := New()
	err := ApplyUpdateV2(doc, update, nil)
	if !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("ApplyUpdateV2 took a %d-byte update whose %s is back-referenced: err = %v, want ErrInvalidUpdate", len(update), what, err)
	}
	if stored := len(EncodeStateAsUpdateV2(doc, nil)); stored > 2*len(update) {
		t.Fatalf("a refused %d-byte update whose %s is back-referenced left a document that encodes to %d bytes, want at most %d", len(update), what, stored, 2*len(update))
	}
}

func TestKeyBackrefBudgetRefusesAFormatKeyExpansion(t *testing.T) {
	// 51 KB on the wire, 50 MB once every format key is written in full.
	requireRefusedExpansion(t, craftKeyBackrefV2(50_000, 1_000, false), "format key")
}

func TestKeyBackrefBudgetRefusesAnXmlNodeNameExpansion(t *testing.T) {
	// 1 KB on the wire, 50 MB once every node name is written in full.
	requireRefusedExpansion(t, craftKeyBackrefV2(50_000, 1_000, true), "XmlElement node name")
}

func TestKeyBackrefBudgetRefusesInEveryV2Decoder(t *testing.T) {
	for _, u := range []struct {
		what   string
		update []byte
	}{
		{"format key", craftKeyBackrefV2(50_000, 1_000, false)},
		{"XmlElement node name", craftKeyBackrefV2(50_000, 1_000, true)},
	} {
		for _, d := range []struct {
			name   string
			decode func([]byte) ([]byte, error)
		}{
			{"MergeUpdatesV2", func(b []byte) ([]byte, error) { return MergeUpdatesV2(b) }},
			{"DiffUpdateV2", func(b []byte) ([]byte, error) { return DiffUpdateV2(b, StateVector{}) }},
			{"EncodeStateVectorFromUpdateV2", EncodeStateVectorFromUpdateV2},
			{"UpdateV2ToV1", UpdateV2ToV1},
			{"ContentIDsFromUpdateV2", func(b []byte) ([]byte, error) {
				_, err := ContentIDsFromUpdateV2(b)
				return nil, err
			}},
		} {
			t.Run(d.name+"/"+u.what, func(t *testing.T) {
				out, err := d.decode(u.update)
				if !errors.Is(err, ErrInvalidUpdate) {
					t.Fatalf("%s took a %d-byte update whose %s is back-referenced (%d bytes out): err = %v, want ErrInvalidUpdate", d.name, len(u.update), u.what, len(out), err)
				}
			})
		}
	}
}

func TestKeyBackrefBudgetTakesBackReferencesWithinTheUpdatesSize(t *testing.T) {
	// Yjs's readKey resolves back-references, so a small one still decodes,
	// and the document round-trips with every key written in full.
	update := craftKeyBackrefV2(10, 1, false)
	doc := New()
	if err := ApplyUpdateV2(doc, update, nil); err != nil {
		t.Fatalf("ApplyUpdateV2 refused a %d-byte update whose 9 back-references add 9 bytes: %v", len(update), err)
	}
	formats := 0
	for _, it := range doc.store.clients[1] {
		if c, ok := it.Content.(*ContentFormat); ok && c.Key == "k" {
			formats++
		}
	}
	if formats != 10 {
		t.Fatalf("integrated %d format structs with key \"k\", want 10", formats)
	}
	stored := EncodeStateAsUpdateV2(doc, nil)
	if backrefs, fresh := keyBackrefs(t, stored); backrefs != 0 || fresh != 10 {
		t.Fatalf("the re-encode has %d back-references and %d keys in full, want 0 and 10", backrefs, fresh)
	}
	again := New()
	if err := ApplyUpdateV2(again, stored, nil); err != nil {
		t.Fatalf("ApplyUpdateV2 of the re-encode: %v", err)
	}
	if got := EncodeStateAsUpdateV2(again, nil); string(got) != string(stored) {
		t.Fatalf("the re-encode does not round-trip: %x, want %x", got, stored)
	}

	// The budget is the update's own length. One-byte node names make every
	// back-reference cost one byte, while the RLE columns keep the update at
	// about 32 bytes, so the last update taken (33 structs) adds exactly its
	// own length and the next is one byte over.
	var taken []byte
	for n := 2; ; n++ {
		u := craftKeyBackrefV2(n, 1, true)
		if added := n - 1; added > len(u) {
			if len(taken) != n-2 {
				t.Fatalf("the %d-byte update before n=%d adds %d bytes, not exactly its length", len(taken), n, n-2)
			}
			doc := New()
			if err := ApplyUpdateV2(doc, taken, nil); err != nil {
				t.Fatalf("ApplyUpdateV2 refused a %d-byte update whose back-references add %d bytes: %v", len(taken), n-2, err)
			}
			doc = New()
			if err := ApplyUpdateV2(doc, u, nil); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("ApplyUpdateV2 took a %d-byte update whose back-references add %d bytes: err = %v, want ErrInvalidUpdate", len(u), added, err)
			}
			return
		}
		taken = u
	}
}

func TestKeyBackrefBudgetHonestDocumentsChargeNothing(t *testing.T) {
	// The budget charges back-references only. The same 5,000 format
	// structs and 1,000-byte keys are taken when every key is written in
	// full (5 MB) and refused when they are back-referenced (6 KB).
	inFull := craftKeyedV2(5_000, 1_000, false, false)
	if backrefs, fresh := keyBackrefs(t, inFull); backrefs != 0 || fresh != 5_000 {
		t.Fatalf("craftKeyedV2 wrote %d back-references and %d keys in full, want 0 and 5000", backrefs, fresh)
	}
	if err := ApplyUpdateV2(New(), inFull, nil); err != nil {
		t.Fatalf("ApplyUpdateV2 refused a %d-byte update that writes all 5000 keys in full: %v", len(inFull), err)
	}
	backrefd := craftKeyBackrefV2(5_000, 1_000, false)
	if backrefs, fresh := keyBackrefs(t, backrefd); backrefs != 4_999 || fresh != 1 {
		t.Fatalf("craftKeyBackrefV2 wrote %d back-references and %d keys in full, want 4999 and 1", backrefs, fresh)
	}
	if err := ApplyUpdateV2(New(), backrefd, nil); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("ApplyUpdateV2 took the same structs as a %d-byte update whose keys are back-referenced: err = %v, want ErrInvalidUpdate", len(backrefd), err)
	}

	// No encoder writes a back-reference, so real documents charge nothing,
	// however often a key repeats.
	honest := map[string][]byte{}

	longKey := strings.Repeat("s", 1_000)
	runs := make([]Delta, 20_000)
	for i := range runs {
		runs[i] = Delta{Op: DeltaOpInsert, Insert: "ab", Attributes: Attributes{longKey: i%2 == 0, "bold": true}}
	}
	formatted := New(WithClientID(7))
	text := formatted.GetText("t")
	formatted.Transact(func(txn *Transaction) { text.ApplyDelta(txn, runs) })
	honest["20,000 runs with a 1,000-byte key and bold"] = EncodeStateAsUpdateV2(formatted, nil)

	tree := New(WithClientID(8))
	frag := tree.GetXmlFragment("x")
	tree.Transact(func(txn *Transaction) {
		for i := 0; i < 1_000; i++ {
			p := NewYXmlElement("paragraph")
			frag.InsertElement(txn, i, p)
			p.SetAttribute(txn, "id", fmt.Sprint(i))
			run := NewYXmlElement("text-run")
			p.InsertElement(txn, 0, run)
			body := NewYXmlText()
			run.InsertText(txn, 0, body)
			body.Insert(txn, 0, "hello", Attributes{"bold": true})
		}
	})
	honest["1,000 paragraphs of repeated XmlElement node names"] = EncodeStateAsUpdateV2(tree, nil)

	fixtures, err := filepath.Glob(filepath.Join("testdata", "*_yjs_fixtures.json"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no testdata/*_yjs_fixtures.json: %v", err)
	}
	for _, path := range fixtures {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			Name string `json:"name"`
			V2   string `json:"v2"`
		}
		if json.Unmarshal(raw, &rows) != nil {
			continue // prelim_yjs_fixtures.json is an object with V1 bytes only
		}
		for _, row := range rows {
			if row.V2 == "" {
				continue
			}
			b, err := hex.DecodeString(row.V2)
			if err != nil {
				t.Fatalf("%s/%s: %v", path, row.Name, err)
			}
			honest[filepath.Base(path)+"/"+row.Name+" (Yjs)"] = b
			doc := New()
			if err := ApplyUpdateV2(doc, b, nil); err != nil {
				t.Fatalf("%s/%s: ApplyUpdateV2: %v", path, row.Name, err)
			}
			honest[filepath.Base(path)+"/"+row.Name+" (ygo re-encode)"] = EncodeStateAsUpdateV2(doc, nil)
		}
	}
	if !hasPrefixKey(honest, "yxml_yjs_fixtures.json/") {
		t.Fatal("no V2 fixture from yxml_yjs_fixtures.json")
	}

	keys := 0
	for name, b := range honest {
		backrefs, fresh := keyBackrefs(t, b)
		if backrefs != 0 {
			t.Errorf("%s: %d key back-references", name, backrefs)
		}
		keys += fresh
		if err := ApplyUpdateV2(New(), b, nil); err != nil {
			t.Errorf("%s: ApplyUpdateV2 refused %d bytes: %v", name, len(b), err)
		}
	}
	if keys < 40_000 {
		t.Fatalf("the honest documents write %d keys, want at least 40000: the scan is not reading them", keys)
	}
}

func hasPrefixKey(m map[string][]byte, prefix string) bool {
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}
