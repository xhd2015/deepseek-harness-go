# testdata

`testdata/sessions/` holds two logs captured from a real DeepSeek Harness
(v4) host. They are committed so the store, projections and paging code is
tested against real event vocabulary instead of only synthetic fixtures.

Both logs were decompressed from `session.v4.jsonl.zstd` and normalized in one
way: the header `id` lost its `session-` prefix, because DSH stores the full
`session-<uuid>` in the header while dsh-go keeps the bare uuid there and adds
the prefix to the directory name. Nothing else was rewritten.

| directory | cwd | events |
| --- | --- | --- |
| `--Users-xhd2015-Projects-xhd2015-working-ai--/session-3defb2dd-…` | `/Users/xhd2015/Projects/xhd2015/working/ai` | 20 |
| `--Users-xhd2015-tmp-tzi--/session-39679a1f-…` | `/Users/xhd2015/tmp/tzi` | 23 |

Between them they cover the preamble (`permission/preset`, `sandbox/mode`,
`approval/policy`, `model/selection`), turns (`turn/start`, `step/start`,
`turn/end`), a user/assistant message pair with `surfaceOp: "append"`, model
selection, and both fallback and provider generated titles.

`session/fixture_test.go` copies this tree into a temporary store root before
reading it, so the fixtures stay pristine.
