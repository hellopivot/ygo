# hellopivot/ygo

A fork of [reearth/ygo](https://github.com/reearth/ygo) that Pivot's blockhead
and robot build against, through a `replace` in Pivot's root `go.mod`. It
follows upstream `main` and carries only the patches below. Keep this list
current: each upstream sync starts by checking whether upstream now covers a
patch, and drops it once it does.

| Patch | Why the fork carries it | Covered upstream? |
| --- | --- | --- |
| `3321804` fix(crdt): support unnamed document roots | Pivot's version 0 blocks and `apps/rest` write a root named `""`. Upstream treats a root with an empty name as detached, so writes into it are lost: on upstream v1.55.0, `GetText("")`, `Insert` "hello", then `ToString()` returns `""`. Also adds `Doc.RootTypeNames`, which blockhead uses to list a document's roots. | No (checked at upstream `510c2cf`, v1.55.0) |
| `70a7443` perf(crdt): find a closer's nearest live marker without rescanning tombstones | Deleting a whole formatted text, as a block restore does, was quadratic in the text's length: 106,000 characters whose formatting changes on every character took about 100 s. | No. Upstream's open #350 rewrites the same code; drop this patch when that lands |

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
