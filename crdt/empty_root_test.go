package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmptyRootNameIsAttached(t *testing.T) {
	doc := New(WithClientID(1))
	text := doc.GetText("")
	doc.Transact(func(txn *Transaction) {
		text.Insert(txn, 0, "hello", nil)
	})

	require.Equal(t, "hello", text.ToString())
	require.Equal(t, []string{""}, doc.RootTypeNames())

	for _, tc := range []struct {
		name   string
		encode func(*Doc, StateVector) []byte
		apply  func(*Doc, []byte, any) error
	}{
		{name: "v1", encode: EncodeStateAsUpdateV1, apply: ApplyUpdateV1},
		{name: "v2", encode: EncodeStateAsUpdateV2, apply: ApplyUpdateV2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update := tc.encode(doc, nil)
			clone := New(WithClientID(2))
			require.NoError(t, tc.apply(clone, update, nil))
			require.Equal(t, []string{""}, clone.RootTypeNames())
			require.Equal(t, "hello", clone.GetText("").ToString())
			require.NotPanics(t, func() { tc.encode(clone, nil) })
		})
	}
}

func TestRootTypeNamesAreSortedAndIncludeEmptyName(t *testing.T) {
	doc := New()
	doc.GetMap("z")
	doc.GetText("")
	doc.GetArray("a")

	require.Equal(t, []string{"", "a", "z"}, doc.RootTypeNames())
}

func TestDetachedPreliminaryTextStillBuffers(t *testing.T) {
	text := NewTextPrelim()
	require.True(t, text.detached())

	text.Insert(nil, 0, "pending", nil)
	require.Len(t, text.pending, 1)

	doc := New()
	array := doc.GetArray("root")
	doc.Transact(func(txn *Transaction) {
		array.PushType(txn, text)
	})
	require.False(t, text.detached())
	require.Equal(t, "pending", text.ToString())
}
