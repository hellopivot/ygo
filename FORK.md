# hellopivot/ygo

A fork of [reearth/ygo](https://github.com/reearth/ygo) that Pivot's blockhead
and robot build against, through a `replace` in Pivot's root `go.mod`. It
follows upstream `main` and carries only the patches below. Keep this list
current: each upstream sync starts by checking whether upstream now covers a
patch, and drops it once it does.

| Patch | Why the fork carries it | Covered upstream? |
| --- | --- | --- |
| `3321804` fix(crdt): support unnamed document roots | Pivot's version 0 blocks and `apps/rest` write a root named `""`. Upstream treats a root with an empty name as detached, so writes into it are lost: on upstream v1.55.0, `GetText("")`, `Insert` "hello", then `ToString()` returns `""`. Also adds `Doc.RootTypeNames`, which blockhead uses to list a document's roots. | No (checked at upstream `510c2cf`, v1.55.0) |
| `fc0ddac` fix(crdt): park structs after a skip instead of advancing over the gap (decoder only) | The decoder half of reearth/ygo#257, applied to the pre-sync base `a0af7dc` so that Pivot could deploy it before the v1.55.0 sync: pods still on the old encoder then read the skips that the v1.55.0 encoder stores, and rolling back to it is safe. | Yes: upstream #257 carries the same decoder hunk, so the merge brings nothing but `crdt/skip_decode_test.go`. Nothing to drop on the next sync. |
| `70a7443` perf(crdt): find a closer's nearest live marker without rescanning tombstones | Deleting a whole formatted text, as a block restore does, was quadratic in the text's length: 106,000 characters whose formatting changes on every character took about 100 s. | No. Upstream's open #350 rewrites the same code; drop this patch when that lands |
| `ded56f3` fix(crdt): keep the struct merged first when updates overlap, as Yjs does | `EncodeStateAsUpdate` merges a document's parked structs into its state with the struct-level merge, which kept the longer of two structs a client has at the same clock and let a struct that starts inside a run of structs cut into the rest of it. A parked struct forged for another client's clocks then replaced that client's integrated characters in the bytes blockhead stores. The merge now follows Yjs `mergeUpdatesV2`: the struct taken first, with the structs that follow it without a gap in its update, stays whole, and a cut struct follows its own client's previous clock. | No (checked at upstream `510c2cf`, v1.55.0): upstream has the same merge. Offer it upstream, and drop this patch once it lands |

## Syncing with upstream

```sh
git remote add upstream https://github.com/reearth/ygo.git
git fetch upstream
git checkout -b chore/sync-upstream-<version> origin/main
git merge upstream/main
GOFLAGS= YGO_REQUIRE_NODE=1 go test -race ./...   # after `npm ci` in testutil/
```

Merge rather than rebase, so the commit Pivot pins stays reachable from
`main`. Merge the pull request with a merge commit for the same reason.
`GOFLAGS=` matters on hosts that set `-trimpath`: the cross-implementation
tests find their JavaScript oracles through source paths.
