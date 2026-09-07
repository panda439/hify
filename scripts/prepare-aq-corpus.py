#!/usr/bin/env python3
"""Rebuild the ignored AQ corpus from the user's hash-frozen chapter snapshots.

No network/model calls, no renumbering, no changes to AI annotations. This
reproduces the supplied source version; it does not claim an upstream revision
or publication license that the original manifest did not record.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]
ANNOTATIONS = ROOT / "eval/annotations/aq-ai-v1"


def sha(data):
    return hashlib.sha256(data).hexdigest()


def prepare(source, output):
    manifest = json.loads((ANNOTATIONS / "manifest.json").read_text())
    coverage = [json.loads(line) for line in (ANNOTATIONS / "coverage.jsonl").read_text().splitlines()]
    by_id = {row["paragraph_id"]: row for row in coverage}
    chapters, paragraphs = [], []
    for entry in manifest["source_files"]:
        name = Path(entry["file"]).name
        path = source / name if source else Path(entry["file"])
        data = path.read_bytes()
        if sha(data) != entry["sha256"]:
            raise ValueError(f"source hash mismatch: {name}")
        chapters.append((name, data))
        parts = re.split(r"^\[(C\d{2}P\d{3})\] ", data.decode("utf-8"), flags=re.M)
        for i in range(1, len(parts), 2):
            paragraph_id, text = parts[i], parts[i + 1].strip()
            row = by_id[paragraph_id]
            if sha(text.encode()) != row["text_sha256"]:
                raise ValueError(f"paragraph hash mismatch: {paragraph_id}")
            paragraphs.append((paragraph_id, text, row["status"] == "excluded_non_narrative"))
    if [p[0] for p in paragraphs] != [r["paragraph_id"] for r in coverage]:
        raise ValueError("paragraph IDs or order differ from frozen coverage")
    included = [p for p in paragraphs if not p[2]]
    if (len(paragraphs), len(included)) != (322, 314):
        raise ValueError("unexpected paragraph counts")
    # Verify everything before writing; preserve exact original chapter bytes.
    output.mkdir(parents=True, exist_ok=True)
    for name, data in chapters:
        (output / name).write_bytes(data)
    text = "\n\n".join(p[1] for p in included) + "\n"
    (output / "narrative-only.txt").write_text(text, encoding="utf-8")
    cursor, mapping = 0, []
    for paragraph_id, paragraph, _ in included:
        mapping.append({"paragraph_id": paragraph_id, "start_rune": cursor,
                        "end_rune": cursor + len(paragraph), "text_sha256": sha(paragraph.encode())})
        cursor += len(paragraph) + 2
    result = {"schema_version": 1, "status": "AI_DRAFT_NOT_HUMAN_GOLD",
              "source_version": [dict(file=name, sha256=sha(data)) for name, data in chapters],
              "paragraphs": 322, "included": 314, "excluded": 8,
              "narrative_sha256": sha(text.encode()), "source_map": mapping}
    (output / "source-map.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    return {key: value for key, value in result.items() if key not in ("source_map", "source_version")}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-dir", type=Path, help="Directory containing the frozen ch01.txt through ch09.txt")
    parser.add_argument("--output-dir", type=Path, default=ROOT / "eval/corpus/aq-010")
    args = parser.parse_args()
    print(json.dumps(prepare(args.source_dir, args.output_dir), ensure_ascii=False))
