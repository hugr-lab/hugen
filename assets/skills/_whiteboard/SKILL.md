---
name: _whiteboard
description: Parent-mediated broadcast channel that lets parallel sub-agents share findings in real time. Bounded retention, ordered delivery.
license: Apache-2.0
allowed-tools:
  - provider: whiteboard
    tools:
      - init
      - write
      - read
      - stop
metadata:
  hugen:
    requires_skills: []
    autoload: true
    autoload_for: [mission, worker]
    tier_compatibility: [root, mission, worker]
compatibility:
  model: any
  runtime: hugen-phase-4
---

# _whiteboard skill

The _whiteboard skill exposes a single primitive: a per-host broadcast
channel that every sub-agent of that host can write to and read from.
The four tools form the smallest surface that supports both fan-out
coordination and per-member auditability.

## When to use a whiteboard

Open one when you are about to spawn parallel sub-agents that benefit
from seeing each other's findings — for example:

- a fan-out investigation where each branch may rule out work the
  others would otherwise duplicate;
- a coordination pattern where one sub-agent's discovery becomes a
  precondition for what the others should look for next;
- any case where the parent doesn't want to fan messages back through
  itself one round-trip at a time.

Do NOT use it as a chat channel. Each broadcast lands in every member's
prompt as a system message. Spam it and the model burns context on
sibling chatter; one focused message per significant event is the
right cadence.

## The four tools

- `whiteboard:init` — open a board on YOUR session. Idempotent;
  re-init on an active board is a no-op. Children spawned AFTER init
  see the board automatically; children spawned before do not.
- `whiteboard:write` — append a broadcast. Caller must be a
  sub-agent whose parent has an active board. Author is auto-stamped
  from your session id. Returns `no_active_whiteboard` if the parent
  closed the board between dispatch and arrival.
- `whiteboard:read` — read the current projection. Returns
  your own hosted board if active, otherwise the parent's board you
  are a member of. Returns `active=false` when neither applies.
- `whiteboard:stop` — close the board you host. New writes
  from members surface `no_active_whiteboard`. Idempotent.

## Caps

- 4 KB per message (truncated with a marker; the truncation flag
  rides the persisted event).
- 100 messages OR 32 KB total in the projection — FIFO eviction. Older
  messages remain in `session_events` and can be retrieved via
  `subagent_runs`.

## How broadcasts surface in your prompt

Every member receives a write as a system message line:

```text
[system: whiteboard] <role> (<session_id>): <text>
```

The line lands in your conversation history before your next prompt
build, so the next time the model thinks it sees the broadcast.
Members do not need to call `whiteboard_read` to see live broadcasts
— `read` is for reviewing the full retained log.

## What this skill does NOT grant

- Sub-agent spawn — combine with `_root` / `_subagent` for that.
- Plan / notepad / parent-context — those are separate skills.

The whiteboard is a coordination primitive, nothing more. Let the
plan track your own work, the notepad your scratch state, and the
whiteboard the things the rest of the fan-out batch needs to know.
