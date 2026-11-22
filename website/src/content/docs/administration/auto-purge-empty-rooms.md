---
title: Purge empty rooms automatically
description: Remove rooms with no local activity automatically
---

Set `room_server.auto_purge_empty_rooms` to make Zendrite purge rooms that have no local activity:

```yaml
room_server:
  auto_purge_empty_rooms: on_all_forgotten
```

| Value | Behaviour |
| --- | --- |
| `never` | Disabled. You can still purge single rooms with [`/admin/purgeRoom`](/administration/adminapi/#post-_zendriteadminpurgeroomroomid). |
| `on_empty` | Purge as soon as no local user has a **joined** membership. Users who left without calling `/forget` lose their pre-leave history access along with the room. |
| `on_all_forgotten` _(default)_ | Purge once no local user has a **non-forgotten** membership row. Users who left without calling `/forget` keep their pre-leave history access through `/messages` until they forget the room. |

The key also accepts a boolean for backwards compatibility: `true` means `on_all_forgotten` and `false` means `never`.

Without purging, the roomserver keeps all state for these rooms (events, memberships, state snapshots, redactions, and more) in the database.
On long-running homeservers, these rooms pile up as users leave or get kicked.
They take up disk space and slow down full-table scans.

## Triggers

In every mode except `never`, two triggers fire:

- **Startup sweep.**
  Each time the roomserver starts, it lists the rooms eligible under the configured mode and schedules a purge for each one.
  This catches rooms that piled up while the feature was off, and rooms left behind when a crash interrupted a purge.
- **Membership change.**
  After each local membership change that can make a room eligible, the roomserver checks the room again.
  These changes are a leave, a ban, or a forget under `on_all_forgotten`.
  If the room now qualifies, the roomserver schedules an asynchronous purge.
  The purge runs in the background after the membership transaction commits.

Before you enable purging, list the rooms with no joined local member with [`GET /_zendrite/admin/emptyRooms`](/administration/adminapi/#get-_zendriteadminemptyrooms).
This endpoint always reports the `on_empty` view, whatever mode you configured.

## Combine with forget on leave

The purge mode works together with [`auto_forget_on_leave`](/administration/auto-forget-on-leave/), which marks rooms as forgotten in the same transaction as the leave:

| `auto_forget_on_leave` | `auto_purge_empty_rooms` | Behaviour |
| --- | --- | --- |
| `false` (default) | `on_all_forgotten` (default) | Users keep history access after leaving until they call `/forget`. Zendrite purges the room once the last non-forgotten row is gone. |
| `true` | `on_empty` _or_ `on_all_forgotten` | Instant cleanup. The last leave forgets the room for the user, and Zendrite purges it immediately. Both modes behave the same here. |
| `false` | `on_empty` | Zendrite purges the room when the last local user leaves, even if they didn't call `/forget`. Their `/messages` access disappears with the room. The advertised `m.forget_forced_upon_leave: false` capability is correct for rooms with several users but doesn't predict the last-user case. See [the `on_empty` mismatch](/administration/auto-forget-on-leave/#the-on_empty-mismatch). |
| any | `never` | Nothing happens automatically. Use [`/admin/purgeRoom`](/administration/adminapi/#post-_zendriteadminpurgeroomroomid) to clean up. |

The default combination (`auto_forget_on_leave: false` with `auto_purge_empty_rooms: on_all_forgotten`) is the most conservative choice that still removes idle rooms.
Users can browse pre-leave history until they forget the room, and Zendrite reclaims the storage once they do.

## Rejoin behaviour

If a local user rejoins a room while Zendrite is still purging it, the join waits up to 30 seconds for the purge to finish.
If the purge doesn't finish in time, the join returns an `M_UNKNOWN` error that asks the client to retry.
This stops a rejoin from racing the purge across components and leaving a half-purged room state.

Zendrite doesn't block remote joins from users on other servers at the federation layer.
Those joins fail because the room state no longer exists locally.
If a local user joins the room later, Zendrite has to discover it again over federation.

## Disable purging

Set `auto_purge_empty_rooms: never` (or `false` in older configs) to turn off both the startup sweep and the membership-change trigger.
You can still purge single rooms with [`POST /_zendrite/admin/purgeRoom/{roomID}`](/administration/adminapi/#post-_zendriteadminpurgeroomroomid).

## Caveats

- The purge doesn't clear `room_aliases` yet.
  Aliases that point at a purged room stay in the alias table and resolve to a room ID that no longer exists.
  The automatic purge inherits this from the [`/admin/purgeRoom`](/administration/adminapi/#post-_zendriteadminpurgeroomroomid) endpoint it reuses.
- The purge does **not** remove media files attached to messages in purged rooms.
- The purge runs the same code as the admin endpoint, so progress reporting and failure modes are the same.
