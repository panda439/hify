# T042 full book run2

- model: qwen2.5:14b
- command: `HIFY_FULLBOOK_MODEL=qwen2.5:14b go test ./internal/knowledge/ -run TestFullBook14B -count=1 -v -timeout 90m`
- migration: `make migrate-up` completed successfully before the run; migration 000018 was applied.
- result: aborted at the requested 60-minute wall-clock limit; test exit code 130.
- started: 2026-09-08 01:34:59 +08:00
- stopped: 2026-09-08 02:34:47 +08:00 (approximately 59m48s; stop signal issued at the limit boundary)
- input: 62 chunks were confirmed by the test log.
- complete ledger/quality/results: unavailable because the test writes them only after `runJob` returns.
- degraded_items: unavailable; the temporary integration database was cleaned after interruption.
- alias_calls_saved: unavailable; previous run had 17 alias attempts, but run2 did not emit its completed attempt ledger.
- observed only in log (not a complete distribution): extraction validation failures, quote unverifiable failures, and alias failures that logged independent-identity publishing.
- raw/test-output.log contains the actual captured tail/log stream.
- prompt, schema, validation, and eval annotations were not modified.
- no accuracy, precision, or recall reported.
