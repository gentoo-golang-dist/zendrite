---
title: Auto-purging empty rooms
description: Automatically remove rooms that have no local members
---

When a Matrix room has no local members joined, the roomserver still holds all of its state (events, memberships, state snapshots, redactions, etc.) in the database.
For long-running homeservers these "empty" rooms accumulate as users leave or are kicked, taking up disk and slowing full-table scans.

Zendrite can purge them automatically.
There are two independent behaviours.

## Event-triggered purge (opt-in)

When a local user transitions out of `join` (leave, ban, or otherwise) and no local members remain joined, the roomserver schedules an asynchronous purge of the room.

This behaviour is controlled by `room_server.auto_purge_empty_rooms` and defaults to `false`:

```yaml
room_server:
  auto_purge_empty_rooms: true
```

The purge runs in the background after the membership transaction commits.
It removes all room data (events, memberships, state, redactions) and notifies downstream components (syncapi, federationapi) to drop their copies.

## Startup sweep (always on)

Independent of the flag above, the roomserver runs a one-shot sweep at every startup.
It enumerates rooms which currently have no local members and schedules a purge for each.

This is intentionally unconditional: empty rooms are pure overhead, and an operator can always inspect candidates before restart via [`GET /_zendrite/admin/emptyRooms`](/administration/adminapi/#get-_zendriteadminemptyrooms).

**Upgrade impact.** When upgrading from a Zendrite version that did not include this feature, expect any pre-existing empty rooms — for example, those left over from previous administrative actions or migrations — to be purged on the first restart.
Use the `emptyRooms` endpoint before restart if you need to inspect them first.

## Rejoin behaviour

If a local user attempts to rejoin a room while its purge is still in flight, the join blocks for up to 30 seconds waiting for the purge to complete.
If the purge does not finish in time, the join returns an `M_UNKNOWN` error suggesting the client retry.
This prevents a rejoin from racing the per-component purge fanout and ending up with a half-purged room state.

Remote joins (made by federated users on other servers) are not blocked at the federation layer; they will fail naturally because the room state no longer exists locally, and the room would have to be re-discovered via federation if any local user later joins.

## Caveats

- `room_aliases` are not currently cleared by the purge.
  Aliases pointing at a purged room remain in the alias table and resolve to a now-nonexistent room ID.
  This is pre-existing behaviour of the underlying [`/admin/purgeRoom`](/administration/adminapi/#post-_zendriteadminpurgeroomroomid) endpoint, which the auto-purge feature reuses.
- Media files attached to messages in purged rooms are **not** removed.
- The purge runs the same code path as the admin endpoint, so progress and failure modes are identical.
