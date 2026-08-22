# .memory/ — corrections inbox

This is the single inbox for durable learned corrections. `corrections.jsonl`
is append-only, one object per line, with all five fields:

```json
{"date":"YYYY-MM-DD","scope":"…","correction":"…","evidence":"…","promoted_to":""}
```

Promote durable rules into [AGENTS.md](../AGENTS.md), a canonical doc under
[`docs/`](../docs/README.md), or a skill, then set `promoted_to`. Never record
credentials, terminal transcripts, or non-public vulnerability details.
