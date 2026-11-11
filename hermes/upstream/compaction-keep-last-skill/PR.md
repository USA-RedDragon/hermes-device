feat(compression): keep the newest skill across compaction

## What

A new option, `compression.keep_last_skill_chars` (default `0`, off). When it is set, the newest skill load (a `skill_view` of SKILL.md, a `/skill` turn, or a section kept by an earlier handoff) is handled in one of two ways at compaction:

- If a kept head or tail row still holds the body, its `[SKILL_PRUNED]` reload markers are dropped from the summary and the kept rows.
- Otherwise, if the body is at most N characters, it rides the handoff verbatim in an `## Active Skill` section, with its linked file names, instead of a reload marker.

Older skills and larger bodies keep their markers.

## Why

A skill loaded early in a session is outside the 10-message just-loaded window and the protected tail. Compaction replaces its body with a `[SKILL_PRUNED]` marker, and the model reloads the whole skill on the next turn. On the first compaction the protected head can also keep a `/skill` turn's body while the `skill_view` result next to it is stubbed with a marker, so the model reloads a body it already has. In a small window a reload is demoted again at the next compaction by the pressure pass or the lean tail.

## How

`agent/compaction_skill_pin.py` finds the newest load in the unpruned transcript and edits the summary and the kept rows after the summary is built. The body stays out of the summarizer's input, including after a restart (previous-summary rehydration), and the salvage cap turns the section back into a marker. When the key is turned off, the next compaction restores the markers. Only the built-in `compress()` path is affected.

## Tests

`tests/agent/test_compaction_skill_pin.py` (12 tests): the default is unchanged, a body rides the handoff, over the cap, newest only, supporting files and dedup results, `/skill` in the middle and in the head, a body still in the tail, linked files, later compactions, the key turned off, the head stub case, rehydration and salvage.
