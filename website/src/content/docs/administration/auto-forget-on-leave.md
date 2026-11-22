---
title: Forget rooms on leave
description: Mark rooms as forgotten automatically when a user leaves
---

Set `auto_forget_on_leave` to make Zendrite forget a room for a user when they leave it:

```yaml
room_server:
  auto_forget_on_leave: true
```

The default is `false`.

Zendrite then marks the room as forgotten in the same database transaction as the leave.
Without the option, clients call `/forget` themselves when the user selects "Forget room?" in the UI.

When the option is on, Zendrite advertises the Matrix 1.18 [`m.forget_forced_upon_leave`](https://spec.matrix.org/v1.18/client-server-api/#mforget_forced_upon_leave-capability) capability through [`/_matrix/client/v3/capabilities`](https://spec.matrix.org/v1.18/client-server-api/#get_matrixclientv3capabilities).
[MSC4267](https://github.com/matrix-org/matrix-spec-proposals/pull/4267) introduced this capability.
Compliant clients then skip the "Forget room?" prompt.

## Leave and forget

Matrix separates two related actions:

- **Leave** is a public state event.
  Everyone in the room sees that the user left.
  The user keeps access to history up to their leave event (through `/messages`, `/context`, and similar endpoints).
  Their client lists the room under "Historical" or "Left rooms" headings.
- **Forget** is a per-user, server-side flag.
  The server stops returning the room in `/sync` for that user.
  The room disappears from the client list, and the user can no longer browse its history.
  A user can only forget a room after they left it or were banned from it.

## Why the default is off

Forgetting is a one-way operation that changes what users see:

- Forgotten rooms disappear from the "archived rooms" and "left rooms" lists in the client.
- A user can no longer call `/messages` or `/context` on a forgotten room to read pre-leave history.
- A kicked or banned user can no longer review the room to understand what happened (the spec covers both transitions).
- If the user rejoins later, the room looks brand new.
  Zendrite has to backfill pre-leave history over federation, which is slow in large rooms.

Turn the option on if you want leaving to clean up the client UI.
Leave it off if users should keep browsing rooms they left, which is the safer default for most homeservers.

## Behaviour

When the option is on, Zendrite marks the membership row as forgotten whenever a **local** user's membership changes to `leave` or `ban`.
This happens in the same transaction, no matter who triggered the change:

- The user calls `/leave`.
- Another user kicks them (sets their membership to `leave`).
- Another user bans them (sets their membership to `ban`).

Zendrite leaves remote users alone.
Their homeserver tracks their forgotten state.

The option interacts with [automatic purging of empty rooms](/administration/auto-purge-empty-rooms/), which has three modes (`never`, `on_empty`, and `on_all_forgotten`):

- With the default purge mode `on_all_forgotten`, every leave also forgets the room.
  The last leave is then also the last forget, and Zendrite purges the room immediately.
- With `on_empty`, Zendrite already purges the room on the last leave regardless of forgotten state.
  Forgetting on leave doesn't change when the purge happens.
  It still removes the leaving user's history access for rooms that stay around (for example, because other local users are still in them).

See [Purge empty rooms automatically](/administration/auto-purge-empty-rooms/) for the full interaction table.

### The `on_empty` mismatch

One combination is inconsistent: `auto_forget_on_leave: false` with `auto_purge_empty_rooms: on_empty`.

- With `auto_forget_on_leave: false`, Zendrite advertises `m.forget_forced_upon_leave: { enabled: false }`.
  A compliant client expects the room to stay after a leave and shows the "Forget room?" prompt.
- With `auto_purge_empty_rooms: on_empty`, Zendrite destroys the room when the last local member leaves.
  To that user, this looks the same as forgetting.

In that case, the client's expectation doesn't match what the server does.

Zendrite still reports the capability as `false`, because that's correct for rooms with several users.
The leaving user keeps history access until they call `/forget`, as advertised.
The mismatch only appears when the leaving user is also the last local member.
If that matters for your deployment, enable `auto_forget_on_leave` so the capability and the behaviour match.

## Capability advertisement

The `/_matrix/client/v3/capabilities` endpoint always reports the current value:

```json
{
  "capabilities": {
    "m.forget_forced_upon_leave": {
      "enabled": true
    }
  }
}
```

Clients show a "Forget room?" prompt after a leave when `enabled` is `false`, and skip it when it's `true`.

## Manual /forget

Clients can still call `/forget` explicitly, whatever this option is set to.
For rooms the server no longer knows about, `/forget` is a no-op.
A client that cached a purged room and tries to forget it gets a clean response, not an error.
