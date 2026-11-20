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
| `53003c2` fix(crdt): refuse key back-references that grow an update past its own size | An update-v2 may write a format key or an XmlElement node name once and back-reference it from every later struct, and every encoder writes each key out in full again when the document is stored: a 51 KB update of back-referenced format keys merged into 50 MB. Each V2 decoder now refuses an update once its back-references would add more bytes than the update carries. Yjs, yrs and ygo since v1.33.0 never write a back-reference, so honest updates are never charged. | No (checked at upstream `4b157ac`, 2026-11-18) |

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
