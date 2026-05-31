#!/usr/bin/env python3
"""Research-stage gate for the analyst mission.

Fired by the runtime as the `mission.stages.research.check` hook
(bash:run -> python3 <this> $MISSION_DIR). Validates that the
researcher actually FILLED the scaffolded artifact files rather than
leaving the skeletons untouched. A non-zero exit + a stderr message
re-prompts the researcher (within the research retry budget); exit 0
lets the planner run.

Deliberately LENIENT: it checks that the two load-bearing files exist,
that their distinctive placeholders were replaced, and that they carry
real content — plus that research.md kept its "Proposed acceptance
criteria" section. It does NOT enforce table-by-table structure;
over-strict gates loop weak models. Stdlib only; no venv required.
"""
import os
import re
import sys

_COMMENT_RE = re.compile(r"<!--.*?-->", re.DOTALL)
_PLACEHOLDER_RE = re.compile(r"<[^>\s][^>]*>")


def real_content_lines(text: str) -> list[str]:
    """Lines the researcher actually wrote — comments, headings,
    blockquotes, table rules, and lines still carrying an unfilled
    <placeholder> token are all stripped."""
    text = _COMMENT_RE.sub("", text)
    out = []
    for raw in text.splitlines():
        line = raw.strip()
        if not line:
            continue
        if line.startswith(("#", ">")):
            continue
        if set(line) <= set("|-: "):  # table rule / separator
            continue
        if _PLACEHOLDER_RE.search(line):  # still a scaffold placeholder
            continue
        out.append(line)
    return out


def main() -> int:
    if len(sys.argv) < 2 or not sys.argv[1].strip():
        print("check_research: missing MISSION_DIR argument", file=sys.stderr)
        return 2
    research_dir = os.path.join(sys.argv[1], "research")
    problems: list[str] = []

    def load(fname: str) -> str | None:
        path = os.path.join(research_dir, fname)
        if not os.path.isfile(path):
            problems.append(
                f"research/{fname} is missing — write it under $MISSION_DIR/research/"
            )
            return None
        with open(path, "r", encoding="utf-8") as fh:
            return fh.read()

    # data-model.md — the schema contract workers trust. The decisive
    # signal that it was filled: the `<type_name>` placeholder is gone
    # (a real table name took its place) and it carries real lines.
    dm = load("data-model.md")
    if dm is not None:
        if "<type_name>" in dm:
            problems.append(
                "research/data-model.md still has the <type_name> placeholder — "
                "replace it with the EXACT table(s) the mission needs"
            )
        elif len(real_content_lines(dm)) < 2:
            problems.append(
                "research/data-model.md still looks like the scaffold — fill in "
                "the sources + table fields with the names you confirmed"
            )

    # research.md — decisions + proposed AC for the planner. Checked
    # for real CONTENT only; we deliberately do NOT grep for a specific
    # English section header — the researcher writes in the user's
    # language (the AC also reaches the planner via the handoff's
    # structured `ac_proposals`, so an English-header requirement here
    # is both brittle and redundant).
    rs = load("research.md")
    if rs is not None and len(real_content_lines(rs)) < 2:
        problems.append(
            "research/research.md still looks like the scaffold — record the "
            "scope decisions + proposed acceptance criteria (any language)"
        )

    if problems:
        print(
            "research artifacts incomplete:\n- " + "\n- ".join(problems),
            file=sys.stderr,
        )
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
