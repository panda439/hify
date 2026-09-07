# Phase 6 evidence

Run date: 2026-09-07. Corpus: the nine chapters of *The True Story of Ah Q*,
322 source paragraphs, 314 included paragraphs, 61 chunks at 500 runes with
100-rune overlap. The ignored local corpus has SHA-256
`157d80e383a35f43b8c572e45e891389caf85f96341e7c1092da54e1cac34484`.

`preflight.json` records one direct structured-output request to each local
Ollama model. The two `*-snapshot.json` files are immutable exports of the
application jobs: model digest, timing, ledger, item outcomes, accepted
relations and every raw transport response. The matching `*-report.json`
files are pure recomputations by `cmd/narrativeeval`.

Both full-input jobs stopped at the configured consecutive-failure guard. The
7B run processed 11/61 chunks (2 succeeded, 9 failed); the 14B run processed
11/61 (3 succeeded, 8 failed). Neither accepted a relation before stopping.
The unfinished 50 chunks therefore remain false negatives in the diagnostic
score. The model outputs commonly changed an exact quote, used a one-based
occurrence, treated a sentence as a character, or returned an unsupported
alias decision. Timeout outcomes remain `unknown`, rather than being counted
as calls that definitely did or did not reach the model.

The reference files are an AI draft. `reference-validation.json` proves only
coverage and source-location consistency; no human reviewer has frozen the
truth set. Consequently the generated precision/recall numbers are diagnostic
pipeline results and do not satisfy FR-016 or SC-007.

The experiment also exposed that restart jobs persisted an empty configuration
snapshot and an all-zero hash. The branch fixes future runs to store a stable,
hashed snapshot of prompt version and effective limits. These two archived
runs retain their observed zero database hash in metadata and separately list
the effective experiment configuration; evidence is not rewritten to hide the
fault.
