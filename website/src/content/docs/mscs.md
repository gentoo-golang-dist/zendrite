---
title: MSC Support
description: Implementation status of Matrix Spec Changes (MSCs) in Zendrite.
---

[MSCs](https://spec.matrix.org/proposals/) (Matrix Spec Changes) are proposals to extend or modify the Matrix protocol.
This page tracks the implementation status of notable MSCs in Zendrite.

Many MSCs listed here have since been merged into the [Matrix specification](https://spec.matrix.org/) proper.
They are listed by their original MSC number and the first spec version which includes them.
Zendrite advertises spec versions up to v1.19; see the [client API conformance audit](/development/client-api-conformance/) for the evidence and the discovery policy.
Open MSCs that clients use and Zendrite does not implement yet are tracked in [issue #558](https://codefloe.com/pat-s/zendrite/issues/558).

## Sync

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC4186](https://github.com/matrix-org/matrix-spec-proposals/pull/4186) | Simplified Sliding Sync | N/A | Implemented (native, no proxy needed) |
| [MSC3575](https://github.com/matrix-org/matrix-spec-proposals/pull/3575) | Sliding Sync (v1) | N/A | Not supported: closed, superseded by MSC4186. Lists return only `count`, without MSC3575 `ops`, and `/.well-known/matrix/client` no longer advertises a sliding sync proxy |
| [MSC4354](https://github.com/matrix-org/matrix-spec-proposals/pull/4354) | Sticky events | N/A | Implemented (unstable prefix): see [sticky events](#sticky-events) |
| [MSC4480](https://github.com/matrix-org/matrix-spec-proposals/pull/4480) | Sliding sync extension: sticky events | N/A | Implemented: the `org.matrix.msc4354.sticky_events` extension |
| [MSC4262](https://github.com/matrix-org/matrix-spec-proposals/pull/4262) | Sliding sync extension: profile updates | N/A | Implemented (unstable prefix): see [profile updates in sliding sync](#profile-updates-in-sliding-sync) |
| [MSC4429](https://github.com/matrix-org/matrix-spec-proposals/pull/4429) | Profile updates for legacy sync | N/A | Implemented (unstable prefix): see [profile updates in `/sync`](#profile-updates-in-sync) |
| [MSC4222](https://github.com/matrix-org/matrix-spec-proposals/pull/4222) | `state_after` in `/sync` | 1.16 | Implemented: with `use_state_after=true`, joined and left rooms carry `state_after`, the state at the end of the timeline, instead of `state`. The unstable `org.matrix.msc4222.use_state_after` parameter is answered with `org.matrix.msc4222.state_after`. See [state after the timeline](#state-after-the-timeline) |
| [MSC4445](https://github.com/matrix-org/matrix-spec-proposals/pull/4445) | Clarify `/sync` timeline order | N/A | Implemented (unstable prefix): the timeline of a room a sync sends for the first time is in topological order, an incremental timeline in stream order. See [timeline order](#timeline-order) |

### Sticky events

`/versions` advertises `org.matrix.msc4354` in `unstable_features`, which Element Call needs before it sends call memberships as sticky events.
`PUT /_matrix/client/v3/rooms/{roomId}/send/{eventType}/{txnId}` accepts `org.matrix.msc4354.sticky_duration_ms` and puts it into the event as `msc4354_sticky.duration_ms`.
As in Synapse, the duration must be a non-negative integer, otherwise the request fails with 400 `M_INVALID_PARAM`, and durations above an hour are kept in the event but count as one hour.
The parameter is ignored when sending state events.

An event stays sticky from the earlier of its `origin_server_ts` and the time Zendrite received it, for its duration of at most an hour.
Redacting an event removes its stickiness.
While an event is sticky, `/sync` delivers it in a `msc4354_sticky.events` section of its joined room, unless the timeline of the same response carries it after the timeline filter.
Each sticky event is delivered once as the client's sync position advances, at most 100 per response, and joining a room delivers all of the room's sticky events at once.
Sticky events of ignored users are left out, and with lazy-loaded members their senders' memberships are included.
Sticky events in the sticky section and in timelines carry `unsigned.msc4354_sticky_duration_ttl_ms`, the milliseconds until they expire.

Sliding sync delivers sticky events through the `org.matrix.msc4354.sticky_events` extension, for every joined room the lists or room subscriptions match, also outside the lists' windows.
The extension takes `since` and `limit` (default 100), and answers with `rooms` and `next_batch`, or is left out when there is nothing to deliver.
Sticky events in the timelines of the response are left out of the extension and carry their TTL in the timeline instead, when the extension is enabled.

History visibility does not hide sticky events from joined users, in `/sync`, sliding sync, `/messages`, `/context`, `/event` and the other endpoints that filter by it.
`/sync` and sliding sync timelines keep the usual rules, as sticky events reach clients through their own section.

When a server joins a room, Zendrite sends it the sticky events that local users sent there and that are still sticky.
Zendrite remembers these events in memory, so after a restart it only sends sticky events created since.
Soft-failed sticky events are not re-evaluated when the room state changes.

### Profile updates in sliding sync

`/versions` advertises `org.matrix.msc4262` in `unstable_features`, and sliding sync serves the `org.matrix.msc4262.profiles` extension with the request and response shape of Synapse.
The extension takes `enabled`, an optional `fields` list (all fields when it is left out), and the usual `lists` and `rooms` filters.
It answers with `users`, which maps a user ID to the fields that were set or changed under `updated` and the fields that were removed under `removed`, each present only when not empty, or to `null` when the syncing user no longer shares any room with that user.
The extension is left out of a response with nothing to deliver.

- The first time a joined room is sent on a connection, or when the `fields` filter changed since it was, the whole profiles of its members are sent.
  When the room's members load lazily (`["m.room.member", "$LAZY"]` or `lazy_members`), only the timeline senders and the member state in the response are sent, along with the room's heroes the first time.
- An initial sync also sends the syncing user's own profile.
- Every change a local user who shares a joined room with the syncing user makes to their profile is sent, also when the room is outside the lists' windows, and so are the syncing user's own changes.
- Users who join a room shared with the syncing user have their whole profile sent.
- A change is acknowledged only when the client syncs from the `pos` of the response that carried it; syncing again from an older `pos` sends it again.

Profile changes reach sliding sync through a profile updates stream in the sync API database, with its own position in the sync token.
Which rooms of a connection have had their members' profiles sent is stored per connection position in the database, like the rest of the connection state, so a successor in a high availability deployment continues from it.

Zendrite does not track the global profiles of remote users.
Their profiles are the display name and avatar URL of their membership event in a shared room, and a change is sent when a new membership event changes them.
Their custom fields are not sent.
Removing custom fields by deactivating an account is not sent.
One response reads at most 1,000 profile changes and 1,000 membership events per room; further profile changes follow in the next response.
`/sync` carries the same profile updates for clients that opt in, see [profile updates in `/sync`](#profile-updates-in-sync).

### Profile updates in `/sync`

`/versions` advertises `org.matrix.msc4429` in `unstable_features`, and `/sync` carries profile updates ([MSC4429](https://github.com/matrix-org/matrix-spec-proposals/pull/4429)) with the request and response shape of Synapse.
A client opts in with the `org.matrix.msc4429.profile_fields` filter field, whose `ids` list the profile fields it wants, for example `{"org.matrix.msc4429.profile_fields": {"ids": ["displayname", "avatar_url", "m.tz"]}}`.
The field works in an inline filter and in one stored with `POST /user/{userId}/filter`.
Without it, or with an empty `ids`, `/sync` sends no profile updates.

The response carries `org.matrix.msc4429.users`, which maps a user ID to `profile_updates`: the requested fields that were set or changed, with `null` for fields that were removed, or `null` instead of the object when the syncing user no longer shares any room with that user.
The key is left out of a response with nothing to deliver.

- An initial sync sends the syncing user's own profile.
- The members whose membership event a joined room of the response carries in its state, or on an initial sync in its timeline, have their requested fields sent.
  Without lazy-loaded members, that is every member of every joined room on an initial sync, and of a room the user just joined.
  With `lazy_load_members`, it is the members the response loads, so a member's profile follows their membership event and is sent again only when a later response loads the member again.
- Every change a local user who shares a joined room with the syncing user makes to a requested field is sent, and so are the syncing user's own changes.
- Users who join a room shared with the syncing user have their requested fields sent.
- The display name and avatar URL of remote users come from their membership events, as in [sliding sync](#profile-updates-in-sliding-sync); their custom fields are not sent.

A long-polling `/sync` that opted in returns when one of these updates arrives.
One that did not opt in, or whose fields did not change, carries the profile position forward and keeps waiting.
The updates reuse the profile updates stream and position of the sliding sync extension, so no extra state is stored.
One response reads at most 1,000 profile changes and 1,000 membership events per room; further profile changes follow in the next response.

### State after the timeline

A client that opts in to [MSC4222](https://github.com/matrix-org/matrix-spec-proposals/pull/4222) must not apply the state events of the timeline itself, so `state_after` holds them as well.
An initial sync, a sync with `full_state=true` and a sync that newly joined a room send the room's whole current state.
An incremental sync sends the state changes between the `since` token and the end of the timeline, including those of a gappy (`limited`) timeline.
For a room the user left, the state ends at their leave.
`state_after` is sent even when it is empty, and the `state` filter applies to it.
With `lazy_load_members`, it holds the memberships of the timeline senders, and an incremental sync keeps every membership change.
Zendrite advertises `org.matrix.msc4222` in `unstable_features` for clients that check it before opting in.

### Timeline order

As in Synapse, the timeline of a room that a sync sends for the first time is a section of the room's history, so it holds the room's latest events in topological order, as `/messages` returns them: by depth, with the order in which the server received them breaking ties.
That covers the rooms of an initial sync (joined, peeked and, with `include_leave`, left rooms, whose timeline ends at the leave), a room that an incremental sync newly joined, and a room that a sliding sync connection sends for the first time or with a larger `timeline_limit`.
Its `prev_batch` continues `/messages` just before the first event of the timeline in that order, and `limited` is set when the room holds older events.
An incremental timeline holds the events that arrived since the `since` token, in the order the server received them, so events that arrive late, such as those of a fork after a netsplit, are sent even when they are older in the DAG.
Backfilled events never appear in `/sync` timelines.
`/versions` advertises `org.matrix.msc4445.initial_sync_timeline_topological_ordering` in `unstable_features`, as Synapse does.

## Messaging

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC2675](https://github.com/matrix-org/matrix-spec-proposals/pull/2675) | Serverside aggregations of message relationships | 1.3 | Implemented: [bundled aggregations](#bundled-aggregations), and `/relations` in topological order with `/messages` and `/sync` tokens |
| [MSC3981](https://github.com/matrix-org/matrix-spec-proposals/pull/3981) | Recursive relations | 1.10 | Implemented: `recurse` follows relations three levels deep |
| [MSC2676](https://github.com/matrix-org/matrix-spec-proposals/pull/2676) | Message editing | 1.4 | Implemented: the latest valid edit is bundled under `m.replace`, in the v1.7 format; invalid edits are ignored, and a redacted original carries no edits |
| [MSC3316](https://github.com/matrix-org/matrix-spec-proposals/pull/3316) | Timestamp massaging | 1.3 | Implemented: the `ts` parameter on `/send`, `/state`, membership endpoints, `/createRoom` and profile updates only applies to requests made with an application service token, and is ignored otherwise |
| [MSC4210](https://github.com/matrix-org/matrix-spec-proposals/pull/4210) | Remove legacy mentions | 1.17 | Implemented: the `.m.rule.contains_display_name`, `.m.rule.contains_user_name` and `.m.rule.roomnotif` push rules are no longer installed, and are dropped from existing accounts' rules once `.m.rule.is_user_mention` and `.m.rule.is_room_mention` have taken over their settings. Only `m.mentions` notifies of a mention. The deprecated `contains_display_name` condition still works in user-defined rules |
| [MSC3958](https://github.com/matrix-org/matrix-spec-proposals/pull/3958) | Suppress notifications from message edits | 1.9 | Implemented: the `.m.rule.suppress_edits` default push rule, also added to existing accounts' rules |
| [MSC3930](https://github.com/matrix-org/matrix-spec-proposals/pull/3930) | Push rules for polls | N/A | Implemented (unstable prefix): see [poll push rules](#poll-push-rules) |
| [MSC4028](https://github.com/matrix-org/matrix-spec-proposals/pull/4028) | Push all encrypted events except for muted rooms | N/A | Opt-in (`msc4028`): see [MSC4028](#msc4028-push-all-encrypted-events) |
| [MSC2677](https://github.com/matrix-org/matrix-spec-proposals/pull/2677) | Reactions | 1.7 | Implemented: relations are stored and served through `/relations`. Sending an annotation that the sender has already made, with the same event type and key on the same event and not redacted, fails with 400 `M_DUPLICATE_ANNOTATION`; retrying the original transaction still returns its event. The spec defines no server-side aggregation for annotations |
| [MSC3970](https://github.com/matrix-org/matrix-spec-proposals/pull/3970) | Scope transaction IDs to devices | 1.7 | Implemented: `/send`, `/redact`, `/sendToDevice`, delayed events and server notices recognise a retransmission by device, endpoint and transaction ID, so it survives a refreshed access token. Application services share one scope per application service, whichever user they act as |
| [MSC3267](https://github.com/matrix-org/matrix-spec-proposals/pull/3267) | Reference relations | 1.5 | Implemented: references are bundled under `m.reference` |
| [MSC3440](https://github.com/matrix-org/matrix-spec-proposals/pull/3440) | Threading via `m.thread` | 1.4 | Implemented: `m.thread` aggregations, `GET /v1/rooms/{roomId}/threads`, and 400 `M_UNKNOWN` for a thread rooted at an event that is itself a relation |
| [MSC3771](https://github.com/matrix-org/matrix-spec-proposals/pull/3771) | Threaded read receipts | 1.4 | Implemented: receipts carry `thread_id` and clear only their thread's notifications |
| [MSC3773](https://github.com/matrix-org/matrix-spec-proposals/pull/3773) | Notifications for threads | 1.4 | Implemented: with `unread_thread_notifications` in the timeline filter, `/sync` reports `unread_thread_notifications` per thread root and `unread_notifications` for the main timeline only |
| [MSC4306](https://github.com/matrix-org/matrix-spec-proposals/pull/4306) | Thread subscriptions | N/A | Implemented (unstable prefix): see [thread subscriptions](#thread-subscriptions) |
| [MSC4308](https://github.com/matrix-org/matrix-spec-proposals/pull/4308) | Sliding sync extension: thread subscriptions | N/A | Implemented (unstable prefix): the `io.element.msc4308.thread_subscriptions` extension and `GET /_matrix/client/unstable/io.element.msc4308/thread_subscriptions` |
| [MSC2836](https://github.com/matrix-org/matrix-spec-proposals/pull/2836) | Threading (generic relationships) | N/A | Opt-in (`msc2836`) |
| [MSC2285](https://github.com/matrix-org/matrix-spec-proposals/pull/2285) | Private read receipts | 1.4 | Implemented |
| [MSC2174](https://github.com/matrix-org/matrix-spec-proposals/pull/2174) | Move the `redacts` property to `content` | 1.8 | Implemented: from room version 11, redactions are applied from `content.redacts` and the top-level key is ignored. Clients receive `redacts` both at the top level and in `content` in every room version |
| [MSC2815](https://github.com/matrix-org/matrix-spec-proposals/pull/2815) | Allow room moderators to view redacted event content | N/A | Implemented (unstable prefix): see [redacted event content for moderators](#redacted-event-content-for-moderators) |
| [MSC4169](https://github.com/matrix-org/matrix-spec-proposals/pull/4169) | Backwards-compatible redaction sending using `/send` | 1.18 | Implemented: `PUT /send/m.room.redaction` copies `content.redacts` to the top level, so it redacts in every room version. Advertised as `com.beeper.msc4169` in `/versions` |

### Bundled aggregations

Zendrite adds `unsigned.m.relations` to events served by `/messages`, `/context`, `/event`, `/relations`, `/search` and `/threads`, and to `/sync` and sliding sync timelines that are `limited`.
An unlimited timeline already carries every child event the client has not seen, so the client aggregates those itself.
Each bundle holds the thread summary (`m.thread`), the most recent valid edit (`m.replace`) and the referencing events (`m.reference`).
Only child events the requesting user may see are aggregated, and events from ignored users are left out.
State events, and aggregations that arrive with events over federation, are never served with bundles.

### Redacted event content for moderators

Zendrite implements MSC2815 with Synapse's unstable names and advertises `fi.mau.msc2815` in `unstable_features`.
When the roomserver applies a redaction, it keeps the event's original JSON in a separate table for `room_server.redaction_retention_period` (7 days by default, as in Synapse).
Everything else, including `/sync`, sliding sync, `/messages`, `/context`, search and federation, keeps getting the redacted event.

`GET /rooms/{roomId}/event/{eventId}?fi.mau.msc2815.include_unredacted_content=true` returns a redacted event with its original content and the redacted event's `unsigned`, including `redacted_because`:

- The requester's power level in the room's current state must be at least the `redact` level; room creators in room versions that give them unlimited power, and server admins, may always ask.
  Anyone else gets 403 `M_FORBIDDEN`, as in Synapse, before the event is looked up.
- History visibility applies as without the parameter: an event the requester may not see is 404 `M_NOT_FOUND`, and events of erased users (MSC4025) stay redacted for those who could not see them when they were sent.
- An event that is not redacted is returned as it is.
- A redacted event whose content the server no longer has gets 404 `FI.MAU.MSC2815_UNREDACTED_CONTENT_DELETED`, with `fi.mau.msc2815.content_keep_ms` set to the retention period unless content is kept until the room is purged.
  That includes events redacted before the upgrade to a version with MSC2815, or while the retention period was `0s`.
- Any value other than `true` or `false` is 400 `M_INVALID_PARAM`.

Content redacted longer ago than the retention period is no longer served, and a job on each roomserver deletes it every five minutes.
`0s` keeps no content, and a negative period keeps it until the room is purged; purging a room deletes its content.
The MSC's `M_UNREDACTED_CONTENT_NOT_RECEIVED` code is not used, as in Synapse: an event that arrived already redacted is returned redacted.

### Thread subscriptions

Zendrite implements MSC4306 with Synapse's unstable names and advertises `org.matrix.msc4306` in `unstable_features`.
`PUT`, `GET` and `DELETE /_matrix/client/unstable/io.element.msc4306/rooms/{roomId}/thread/{eventId}/subscription` subscribe to, read and remove the user's subscription to a thread.
The thread root must be an event of the room that the user may see under history visibility; otherwise, and for unknown events, the answer is 404 `M_NOT_FOUND`.
`GET` also answers 404 when the user is not subscribed, and `DELETE` succeeds when there is nothing to remove.

A `PUT` body without `automatic` is a manual subscription, which replaces an automatic one and is never replaced by one.
With `"automatic": "$eventId"`, the event must be a visible `m.thread` reply to the root, or the answer is 400 `IO.ELEMENT.MSC4306.M_NOT_IN_THREAD`.
Unsubscribing records the sync stream position of the room's latest event.
An automatic subscription whose cause event is at or before that position, that is an event the server had already received when the user unsubscribed, is refused with 409 `IO.ELEMENT.MSC4306.M_CONFLICTING_UNSUBSCRIPTION`, so a device that missed the unsubscription cannot undo it.
Subscriptions live in the userapi database; concurrent changes are serialized, and each change gets a position in the thread subscriptions stream.

Push rules gain the `postcontent` kind, evaluated after `content` and before `room`, with two default rules that use the `io.element.msc4306.thread_subscription` condition:
`.io.element.msc4306.rule.unsubscribed_thread` keeps replies in threads the user is not subscribed to from notifying or counting as unread, and `.io.element.msc4306.rule.subscribed_thread` notifies with the default sound for threads the user is subscribed to, even in a room set to mentions only.
Mentions and keyword rules still run first.
Existing accounts receive both rules when their push rules are next read; a rule the user disabled stays disabled.
Clients cannot create `postcontent` rules: `PUT /pushrules/global/postcontent/{ruleId}` answers 400 `M_INVALID_PARAM`.
The condition can be used in user-defined `override` and `underride` rules.

Sliding sync reports changes through the `io.element.msc4308.thread_subscriptions` extension (`enabled`, `limit`, 100 by default and at most 512).
Its `subscribed` and `unsubscribed` maps hold the latest state of each changed thread since the request's `pos`, with the stream position as `bump_stamp`.
An initial request returns the latest changes of all time.
When older changes were left out, `prev_batch` is a token for `GET /_matrix/client/unstable/io.element.msc4308/thread_subscriptions?dir=b&from=...&to=...`, which pages backwards with the same limits and returns `end` while there may be more; `to` takes a thread subscriptions token or a sliding sync `pos`.
A change wakes the user's waiting sliding sync requests, and the `/sync` token carries the stream's position without returning its changes.

## Rooms & Spaces

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC1772](https://github.com/matrix-org/matrix-spec-proposals/pull/1772) | Spaces | 1.2 | Implemented |
| [MSC2946](https://github.com/matrix-org/matrix-spec-proposals/pull/2946) | Spaces summary / room hierarchy | 1.2 | Implemented |
| [MSC3827](https://github.com/matrix-org/matrix-spec-proposals/pull/3827) | Filtering of `/publicRooms` by room type | 1.4 | Implemented: client and federation `/publicRooms` accept `room_types` (`null` matches rooms without a type) and return `room_type`. Results from remote servers are filtered again, for servers that ignore the filter |
| [MSC2432](https://github.com/matrix-org/matrix-spec-proposals/pull/2432) | Publishing room aliases | 1.2 | Implemented: new canonical and alternative aliases resolve locally or over federation; retained aliases are not revalidated |
| [MSC3083](https://github.com/matrix-org/matrix-spec-proposals/pull/3083) | Restricted rooms (space-based membership) | 1.2 | Implemented: joins distinguish uncheckable conditions (`M_UNABLE_TO_AUTHORISE_JOIN`) from a missing local authorising user (`M_UNABLE_TO_GRANT_JOIN`) |
| [MSC2403](https://github.com/matrix-org/matrix-spec-proposals/pull/2403) | Knocking | 1.1 | Implemented: `/knock`, `make_knock` and `send_knock`, `rooms.knock` in `/sync` and knocked rooms in sliding sync. A knock is rescinded with `/leave`. Kicks and bans that end an invite or knock are also sent to the target's server when it is not in the room |
| [MSC2367](https://github.com/matrix-org/matrix-spec-proposals/pull/2367) | Reasons in all membership events | 1.1 | Implemented: `/join`, `/leave`, `/invite`, `/kick`, `/ban`, `/unban` and `/knock` accept `reason`. A leave through another server adds the reason to the `make_leave` template |
| [MSC3820](https://github.com/matrix-org/matrix-spec-proposals/pull/3820) | Room version 11 | 1.8 | Implemented: includes the redaction changes of MSC2174 and MSC3821 |
| [MSC3266](https://github.com/matrix-org/matrix-spec-proposals/pull/3266) | Room summary API | 1.15 | Implemented: `GET /_matrix/client/v1/room_summary/{roomIdOrAlias}`, and client and federation `/hierarchy` rooms carry `room_version` and, for encrypted rooms, `encryption`. Summaries of remote rooms keep both fields when the remote server sends them |
| [MSC3765](https://github.com/matrix-org/matrix-spec-proposals/pull/3765) | Rich text in room topics | 1.15 | Implemented: `createRoom` with `topic` adds a `text/plain` representation in `m.topic`. `/publicRooms`, `/hierarchy`, room summaries and `/search` use the first `text/plain` representation in `m.topic` (a missing `mimetype` counts as `text/plain`) and fall back to the `topic` key |
| [MSC4267](https://github.com/matrix-org/matrix-spec-proposals/pull/4267) | Forget room on leave ([`m.forget_forced_upon_leave` capability](https://spec.matrix.org/v1.18/client-server-api/#mforget_forced_upon_leave-capability)) | 1.18 | Implemented (opt-in via [`auto_forget_on_leave`](/administration/auto-forget-on-leave/)) |
| [MSC4380](https://github.com/matrix-org/matrix-spec-proposals/pull/4380) | Invite blocking | 1.18 | Implemented: see [invite blocking](#invite-blocking) |
| [MSC4155](https://github.com/matrix-org/matrix-spec-proposals/pull/4155) | Invite filtering | N/A | Implemented (unstable prefix): see [invite filtering](#invite-filtering) |
| [MSC4491](https://github.com/matrix-org/matrix-spec-proposals/pull/4491) | Invite reasons in room creation | N/A | Implemented (unstable prefix): see [invite reasons in room creation](#invite-reasons-in-room-creation) |
| [MSC2666](https://github.com/matrix-org/matrix-spec-proposals/pull/2666) | Mutual rooms | 1.19 | Implemented: `GET /_matrix/client/v1/mutual_rooms` lists the rooms the caller shares with a user, with signed pagination tokens. Also advertised as `uk.half-shot.msc2666.query_mutual_rooms.stable` |
| [MSC4423](https://github.com/matrix-org/matrix-spec-proposals/pull/4423) | Server-defined room directory order | 1.19 | Implemented: the client and federation room directories list rooms by room ID |
| [MSC4289](https://github.com/matrix-org/matrix-spec-proposals/pull/4289), [MSC4291](https://github.com/matrix-org/matrix-spec-proposals/pull/4291), [MSC4297](https://github.com/matrix-org/matrix-spec-proposals/pull/4297), [MSC4304](https://github.com/matrix-org/matrix-spec-proposals/pull/4304), [MSC4307](https://github.com/matrix-org/matrix-spec-proposals/pull/4307) | Room version 12 | 1.16 | Implemented and the default room version: create event room IDs, privileged creators, the new state resolution and same-room auth events. `trusted_private_chat` invitees become `additional_creators`, upgrades accept `additional_creators`, and the initial power levels set `m.room.tombstone` to 150 |

### Invite blocking

A user who sets the `m.invite_permission_config` account data to `{"default_action": "block"}` receives no room invites.
Inviting them through `POST /rooms/{roomId}/invite`, the `invite` list of `POST /createRoom`, `PUT /rooms/{roomId}/state/m.room.member/{userId}` or the federation `/invite` endpoints fails with 403 `M_INVITE_BLOCKED`.
`createRoom` still creates the room, as with any other failed invite.
`/sync` and sliding sync leave out invites the user already has, and serve them again once blocking is turned off; other endpoints and application services see invites as usual.
Any other `default_action`, or no account data, allows invites.
The unstable `org.matrix.msc4380.invite_permission_config` type is read when the stable type is not set, and `/versions` advertises `org.matrix.msc4380` and `org.matrix.msc4380.stable`.
An empty `m.invite_permission_config` (`{}`) states no preference, so [invite filtering](#invite-filtering) applies.

### Invite reasons in room creation

`POST /createRoom` accepts `uk.timedout.msc4491.invite_reason`, a string that becomes the `reason` of every invite the `invite` list sends, to local and to remote users alike, next to `is_direct`.

```json
{
  "invite": ["@bob:example.org"],
  "is_direct": true,
  "uk.timedout.msc4491.invite_reason": "About Friday's release"
}
```

- As in Synapse, an empty reason, or one that is not a string, is ignored: the invites go out without a reason.
- Only the unstable name is read while the MSC is open; the stable `invite_reason` is ignored, as in Synapse.
- A reason longer than 65536 bytes could never fit in an event, so it is refused with 413 `M_TOO_LARGE` before the room is created.
  Synapse creates the room and then fails on the oversized invite.
- `/versions` always advertises `uk.timedout.msc4491.create_room_invite_reasons`.
  Synapse advertises it, and reads the field, only with `experimental_features.msc4491_enabled`.

### Invite filtering

The `org.matrix.msc4155.invite_permission_config` account data lists which users and servers may invite the user, as in Synapse.
Only the unstable type is read: the stable `m.invite_permission_config` belongs to [invite blocking](#invite-blocking).

```json
{
  "allowed_users": ["@goodguy:badguys.org"],
  "ignored_users": [],
  "blocked_users": ["@spam*:*"],
  "allowed_servers": ["goodguys.org"],
  "ignored_servers": ["reallybadguys.org"],
  "blocked_servers": ["*"]
}
```

- Entries are globs: `*` matches any number of characters and `?` exactly one, ignoring case.
  `*_users` globs match the inviter's user ID, `*_servers` globs its server name, with or without the port.
- The user lists are checked before the server lists, each in the order allowed, ignored, blocked.
  The first match decides; without one, the invite is allowed.
  So an allowed user can invite even from a blocked server, and a blocked user cannot invite from an allowed server.
- A blocked invite is refused with 403 `M_INVITE_BLOCKED` on `POST /rooms/{roomId}/invite`, the `invite` list of `POST /createRoom`, `PUT /rooms/{roomId}/state/m.room.member/{userId}` and the federation `/invite` endpoints.
  `createRoom` still creates the room, as with any other failed invite.
- An ignored invite is accepted, so the inviter learns nothing, but `/sync` and sliding sync leave it out and it does not notify.
  Other endpoints, such as the room state, and application services see it as usual.
- `/sync` and sliding sync also leave out invites the user already has and now ignores or blocks, and serve them again once the configuration allows them.
  As with `m.ignored_user_list`, a client should run an initial `/sync` after relaxing the configuration.
- Sliding sync also leaves out invites from users in `m.ignored_user_list`, as `/sync` already did.
- `"enabled": false` turns the configuration off without removing it.
- Entries that are not strings, empty or longer than 255 bytes are skipped, and so are fields that are not lists.
- A stated [invite blocking](#invite-blocking) preference takes precedence: `m.invite_permission_config` (or its unstable MSC4380 type) with any `default_action` decides alone, and the MSC4155 lists apply only without one.
- `/versions` advertises `org.matrix.msc4155`.

Differences from Synapse:

- Synapse does not read `enabled`, which the MSC defines.
- Synapse matches server globs against the server name with its port only, and the MSC without it; both forms match here.
- Synapse lets server admins invite past the invitee's configuration; here admins are filtered like any other user, as for invite blocking.
- Synapse's `/sync` and sliding sync hide only ignored invites; here pending invites that are now blocked are hidden too, as the MSC recommends and as invite blocking already does.

## Registration

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC3231](https://github.com/matrix-org/matrix-spec-proposals/pull/3231) | Token authenticated registration | 1.2 | Implemented: registration tokens and the rate-limited, unauthenticated `/v1/register/m.login.registration_token/validity` check |
| [MSC1692](https://github.com/matrix-org/matrix-spec-proposals/pull/1692) | Terms of service at registration | 1.11 | Implemented (opt-in): with `client_api.registration_terms`, every registration flow ends with `m.login.terms`, whose params list the policies. The stage also has a fallback page at `/_matrix/client/v3/auth/m.login.terms/fallback/web` |

## Accounts

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC1929](https://github.com/matrix-org/matrix-spec-proposals/pull/1929) | Homeserver admin contact and support page | 1.10 | Implemented: `/.well-known/matrix/support` serves `global.support` and returns 404 when it is unset |
| [MSC4010](https://github.com/matrix-org/matrix-spec-proposals/pull/4010) | Push rules and account data | 1.10 | Implemented: setting `m.push_rules` or `m.fully_read` through `/account_data` fails with 405 `M_BAD_JSON`, for global and room account data |
| [MSC4025](https://github.com/matrix-org/matrix-spec-proposals/pull/4025) | Local user erasure requests | 1.10 | Implemented: see [erasure](#erasure) |
| [MSC3939](https://github.com/matrix-org/matrix-spec-proposals/pull/3939) | Account locking | 1.12 | Implemented: see [account locking](#account-locking) |
| [MSC3823](https://github.com/matrix-org/matrix-spec-proposals/pull/3823) | Account suspension | 1.13 | Implemented: see [account suspension](#account-suspension) |
| [MSC4151](https://github.com/matrix-org/matrix-spec-proposals/pull/4151) | Reporting rooms | 1.13 | Implemented: `POST /_matrix/client/v3/rooms/{roomId}/report` stores a report with a `reason`, which may be empty, for rooms this server knows, whether or not the reporter is in them; unknown rooms return 404 `M_NOT_FOUND`. Administrators list reports with `GET /_synapse/admin/v1/room_reports` (see the [admin API](/administration/adminapi/)) |
| [MSC4260](https://github.com/matrix-org/matrix-spec-proposals/pull/4260) | Reporting users | 1.14 | Implemented: `POST /_matrix/client/v3/users/{userId}/report` stores a report with a `reason`, which may be empty, whether or not the reporter shares a room with the user. Local users without an account return 404 `M_NOT_FOUND`; remote users cannot be checked, so any valid remote user ID is accepted. Administrators list reports with `GET /_synapse/admin/v1/user_reports` (see the [admin API](/administration/adminapi/)) |
| [MSC4323](https://github.com/matrix-org/matrix-spec-proposals/pull/4323) | User suspension and locking endpoints | 1.18 | Implemented: `GET` and `PUT /_matrix/client/v1/admin/lock/{userId}` and `/admin/suspend/{userId}`, and `/capabilities` reports `m.account_moderation` to administrators. With [MSC3861](#msc3861-oidc-delegated-authentication), only suspension is available (`lock: false`) |

### Erasure

`POST /_matrix/client/v3/account/deactivate` with `"erase": true`, and the MAS `delete_user` call with `erase`, mark the user as erased in `roomserver_erased_users`.
From then on, their events are served redacted to clients and servers that see them only through `shared` or `world_readable` history visibility, in `/messages`, `/context`, `/event`, `/relations`, threads, `/sync` timelines, and federation `/event`, `/backfill` and `/get_missing_events`.
A user who was joined when an event was sent, or invited under `invited` visibility, still sees it unredacted, as does a server that had such a user; this follows Synapse and approximates "joined before the erasure".
No redaction events are sent and nothing is shared over federation, so other servers may still serve unredacted copies.
Erasure also deletes the user's account data and 3PIDs, after their identity server bindings have been removed.
Reactivating the account lifts the erasure, as in Synapse.

Deactivation, with or without `erase`, unbinds the user's 3PIDs from `id_server`, or from every identity server that holds a binding, and reports `id_server_unbind_result`.

### Account locking

An administrator locks or unlocks an account with `PUT /_matrix/client/v1/admin/lock/{userId}` and `{"locked": true}` or `{"locked": false}`, and reads the state with `GET` on the same path (see the [admin API](/administration/adminapi/)).
A locked account keeps its sessions, but every authenticated request apart from `POST /logout` and `POST /logout/all` fails with 401 `M_USER_LOCKED` and `soft_logout: true`.
`/login` and `/refresh` refuse locked accounts the same way, after checking the credentials, and `/refresh` leaves the tokens untouched so the client can retry once the account is unlocked.
Unlocking restores the existing sessions without a new login.
Administrators cannot lock themselves or another administrator.
With [MSC3861](#msc3861-oidc-delegated-authentication), the endpoints return 403: lock the account in the authentication service instead.

### Account suspension

An administrator suspends or unsuspends an account with `PUT /_matrix/client/v1/admin/suspend/{userId}` and `{"suspended": true}` or `{"suspended": false}`, and reads the state with `GET` on the same path.
A suspended account can still log in, sync, read history, manage its devices, keys and key backup, leave rooms, redact its own events and deactivate itself.
It gets 403 `M_USER_SUSPENDED` when it joins, knocks, invites, kicks, bans or unbans, creates or upgrades rooms, sends any event other than a redaction of its own events or its own leave, or changes its profile.
Administrators cannot suspend themselves or another administrator.
Suspension also works with [MSC3861](#msc3861-oidc-delegated-authentication), since the authentication service has no equivalent.

## Profiles

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC3550](https://github.com/matrix-org/matrix-spec-proposals/pull/3550) | HTTP 403 for profile lookups | 1.2 | Implemented: remote lookup restrictions return 403 `M_FORBIDDEN`; local profiles remain unrestricted |
| [MSC4133](https://github.com/matrix-org/matrix-spec-proposals/pull/4133) | Extended profiles: custom profile fields | 1.16 | Implemented: see [custom profile fields](#custom-profile-fields) |
| [MSC4175](https://github.com/matrix-org/matrix-spec-proposals/pull/4175) | Time zone profile field (`m.tz`) | 1.16 | Implemented: `m.tz` must be a string |

### Custom profile fields

`GET`, `PUT` and `DELETE /_matrix/client/v3/profile/{userId}/{keyName}` read, set and remove any profile field, and `GET /_matrix/client/v3/profile/{userId}` returns them all.
The same endpoints are served under `/_matrix/client/unstable/uk.tcpip.msc4133/`, and `/versions` advertises `uk.tcpip.msc4133` and `uk.tcpip.msc4133.stable`.
`/capabilities` reports `m.profile_fields` (and `uk.tcpip.msc4133.profile_fields`) as enabled without restrictions, together with `m.set_displayname` and `m.set_avatar_url`.

A custom field name is a namespaced identifier such as `org.example.job_title`, at most 255 bytes long, and its value can be any JSON.
A profile, including its display name and avatar URL, is limited to 64 KiB of canonical JSON; a change that would exceed this fails with `M_PROFILE_TOO_LARGE`.
Only the display name and avatar URL are copied into membership events; deleting either one clears it.
Guests may only change their display name.
`/_matrix/federation/v1/query/profile` serves every field, also one at a time via `field`, and profiles of remote users include their custom fields.
Deactivating an account removes its custom fields.

## Media

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC2246](https://github.com/matrix-org/matrix-spec-proposals/pull/2246) | Asynchronous media uploads | 1.7 | Implemented: see [asynchronous uploads](#asynchronous-uploads) |
| [MSC2701](https://github.com/matrix-org/matrix-spec-proposals/pull/2701), [MSC2702](https://github.com/matrix-org/matrix-spec-proposals/pull/2702) | `Content-Type` and `Content-Disposition` in the media repo | 1.12 | Implemented: uploads without a `Content-Type` are stored as `application/octet-stream`, which is also served for older media stored without a type. Downloads are `inline` only for the spec's list of safe types and `attachment` otherwise. Thumbnails are served `inline` with a file name such as `thumbnail.jpg`; an original that answers a thumbnail request itself stays an `attachment` unless its type is safe |
| [MSC2705](https://github.com/matrix-org/matrix-spec-proposals/pull/2705) | Animated thumbnails | 1.11 | Implemented: without `animated=true`, thumbnails are never animated; an animated GIF, APNG or WebP that would answer the request itself is replaced by a still JPEG rendition. With `animated=true`, an animated image no larger than the requested size is served as it is, and larger ones get a still thumbnail, which the spec allows |
| [MSC3860](https://github.com/matrix-org/matrix-spec-proposals/pull/3860) | Media download redirects | 1.7 | Implemented: `allow_redirect` is accepted, and Zendrite always serves media itself, which the spec allows. When a remote server answers a federation media request with a `Location` instead of the media, Zendrite fetches it from there, following up to five more redirects, and only from public addresses |

### Asynchronous uploads

`POST /_matrix/media/v1/create` reserves an `mxc://` URI and returns it with `unused_expires_at`, after which an unused reservation lapses (`media_api.async_uploads.unused_expiry`, default `24h`).
A user may hold `media_api.async_uploads.max_pending_per_user` reservations (default 10) that are not uploaded or expired; beyond that, `/create` fails with 429 `M_LIMIT_EXCEEDED`.
`PUT /_matrix/media/v3/upload/{serverName}/{mediaId}` uploads the content.
Only the user who created the URI may upload (403 `M_FORBIDDEN`), only once (409 `M_CANNOT_OVERWRITE_MEDIA`), and only before it expires (404 `M_NOT_FOUND`).

Downloads and thumbnails of a reserved URI, over the client and federation APIs, wait for the upload for `timeout_ms` (default 20 seconds, at most `media_api.async_uploads.max_download_wait`, default `60s`), and then fail with 504 `M_NOT_YET_UPLOADED`.
A download waiting on one replica sees an upload completed on another within half a second.
Expired reservations are deleted every hour.
When a remote server answers 504 for its own media, the client gets 504 `M_NOT_YET_UPLOADED` too, and nothing is cached.

### URL previews

`GET /_matrix/client/v1/media/preview_url` (and the deprecated `/_matrix/media/v3/preview_url`) is opt-in with `media_api.url_preview.enabled`, as in Synapse, since it makes the server fetch URLs for its users; disabled, it answers 404 `M_UNRECOGNIZED`.
For an HTML page it returns the `og:` properties of the page's head, falling back to `<title>` and the `description` meta tag.
`og:image` is downloaded and stored as local media, and the preview carries its `mxc://` URI with `matrix:image:size`, `og:image:type`, `og:image:width` and `og:image:height`; a URL that points at an image gets the same fields for the image itself.

Only public addresses are fetched: loopback, private, link-local, carrier-grade NAT and similar addresses are refused with 403 `M_FORBIDDEN`, also after redirects (at most five) and DNS changes, because the check applies to the address being connected to.
`ip_range_whitelist` allows non-public ranges such as an internal wiki, and `ip_range_blacklist` refuses more ranges and takes precedence.
Pages are read up to `max_page_size_bytes` (default 10 MiB) and images up to `max_file_size_bytes`; a failed fetch returns 502.
Previews are cached in memory per URL for `cache_duration` (default `1h`), concurrent requests for the same URL share one fetch, and `ts` is ignored, which the spec allows.

## Encryption & Security

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC1219](https://github.com/matrix-org/matrix-spec-proposals/pull/1219) | Server-side key backup | 1.1 | Implemented: backup versions, and uploading, fetching and deleting keys for a version, a room or a session |
| [MSC3916](https://github.com/matrix-org/matrix-spec-proposals/pull/3916) | Authenticated media | 1.11 | Implemented, including [URL previews](#url-previews). A `Location` in a remote multipart response is followed, see [MSC3860](#media) |
| [MSC2732](https://github.com/matrix-org/matrix-spec-proposals/pull/2732) | OLM fallback keys | 1.2 | Implemented: ordinary and dehydrated devices upload reusable fallback keys; claims mark them used and sync reports unused algorithms |
| [MSC3814](https://github.com/matrix-org/matrix-spec-proposals/pull/3814) | Dehydrated devices v2 | N/A | Opt-in (`msc3814`) |
| [MSC4115](https://github.com/matrix-org/matrix-spec-proposals/pull/4115) | Membership metadata on events | 1.11 | Implemented: `unsigned.membership` is the requesting user's membership at each event, including the change made by the user's own membership events, wherever events pass the history visibility filter (sync timelines, `/messages`, `/context`, `/event`, `/relations`, threads). Events without a known membership, such as the `state` section of `/sync`, carry none, which the spec allows |

## Federation

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC3706](https://github.com/matrix-org/matrix-spec-proposals/pull/3706) | Partial state in `/send_join` (faster joins) | 1.6 | Implemented: Zendrite joins with `omit_members=true`. Answering `send_join` with partial state, `members_omitted` and `servers_in_room`, including the heroes' memberships of [MSC3943](https://github.com/matrix-org/matrix-spec-proposals/pull/3943), is opt-in (`federation_api.serve_partial_state_joins`); otherwise the state is full, which the spec allows. See [faster joins](#faster-joins) |
| [MSC3902](https://github.com/matrix-org/matrix-spec-proposals/pull/3902) | Faster remote room joins | N/A | Partial: Zendrite joins with partial state and resyncs the full state in the background, but does not follow the MSC's client behaviour. See [faster joins](#faster-joins) |
| [MSC3030](https://github.com/matrix-org/matrix-spec-proposals/pull/3030) | Jump to date API endpoint | 1.6 | Implemented: see [jump to date](#jump-to-date) |
| [MSC3938](https://github.com/matrix-org/matrix-spec-proposals/pull/3938) | Remove deprecated `keyId` parameters from `/keys` endpoints | 1.6 | Implemented: `GET /_matrix/key/v2/query/{serverName}` returns all keys of the server. The deprecated forms with a key ID are still served |
| [MSC2444](https://github.com/matrix-org/matrix-spec-proposals/pull/2444) | Peeking over federation | N/A | Opt-in (`msc2444`) |
| [MSC4436](https://github.com/matrix-org/matrix-spec-proposals/pull/4436) | Case-insensitive server ACLs | 1.19 | Implemented: `allow` and `deny` globs match the whole server name, ignoring case |
| [MSC4284](https://github.com/matrix-org/matrix-spec-proposals/pull/4284) | Policy servers | 1.18 | Implemented: see [policy servers](#policy-servers) |
| [MSC2753](https://github.com/matrix-org/matrix-spec-proposals/pull/2753) | Peeking via `/sync` | N/A | Opt-in (`msc2753`) |
| [MSC3821](https://github.com/matrix-org/matrix-spec-proposals/pull/3821) | Update redaction rules, again | 1.8 | Implemented: from room version 11, redacting an `m.room.member` event keeps `content.third_party_invite.signed`, so redacted events hash to the same event ID as on other servers |
| [MSC4311](https://github.com/matrix-org/matrix-spec-proposals/pull/4311) | Create event in invite and knock stripped state | 1.16 | Implemented: `invite_room_state` and `knock_room_state` are sent as full PDUs including `m.room.create`. From room version 12, invites whose state holds PDUs that fail validation are rejected with `M_MISSING_PARAM`. Missing or stripped-only state is accepted with a warning, per the MSC's migration section, as are all failures in earlier room versions. Knock state entries that fail validation are dropped |

### Policy servers

A room names a policy server with an `m.room.policy` state event (empty state key) whose content holds the server in `via` and its signing key in `public_keys.ed25519`.
The policy server only counts while at least one of its users is joined to the room.
The unstable `org.matrix.msc4284.policy` event, which may carry the key as `public_key`, is read when the room has no `m.room.policy` event.
An `m.room.policy` event with any other content, such as `{}`, turns the policy server off.

While a room has a policy server, every new event must carry the policy server's signature, made with the key from the room's current state under the key ID `ed25519:policy_server`.
Zendrite asks for a missing signature with `POST /_matrix/policy/v1/sign`, or the unstable `/_matrix/policy/unstable/org.matrix.msc4284/sign` when the policy server does not know the stable endpoint, and keeps the signature with the event.
Answers are cached per event for 10 minutes, and a request gives up after 10 seconds.
The `m.room.policy` events themselves are never checked, so that a room can always replace or turn off a policy server that stopped working.

Zendrite fails closed:

- An event of a local user that the policy server refuses is neither stored nor sent, and the client gets 400 with the policy server's error, such as `M_FORBIDDEN`.
  When the policy server cannot be reached, rate limits Zendrite, or answers with an invalid signature, the client gets 503 `M_UNKNOWN`, and can retry.
  This applies to sending events and state, redactions, joins, knocks, invites, delayed events and profile updates.
  A user's own leave, invite rejection or knock withdrawal without a reason is the exception: its content is only `{"membership": "leave"}`, so it is sent without the signature and nobody is trapped in a room, and other servers may soft fail it.
- A remote event that lacks a valid signature, and for which Zendrite cannot get one, is [soft failed](https://spec.matrix.org/latest/server-server-api/#soft-failure): it is stored, but not shown to users or used as current state.
  `send_join`, `send_leave` and `send_knock` fail with the same errors.

Events in rooms with partial state from a faster join, and local events that are not sent to other servers, are not checked.
Zendrite does not act as a policy server itself, and does not serve `/.well-known/matrix/policy_server`.

### Jump to date

`GET /_matrix/client/v1/rooms/{roomId}/timestamp_to_event` returns the event closest to `ts` in the direction `dir` that the user may see.
`GET /_matrix/federation/v1/timestamp_to_event/{roomId}` answers the same question for another server, from local events only.
It returns the closest event the requesting server may see, checking at most 1000 events, and a server banned by the room's ACL gets 403.

The local history of a room can have gaps, for example before the server joined it.
Zendrite knows them from the backward extremities, the events whose `prev_events` it never fetched.
When such a gap can hold an event closer to `ts` than the local result, and the user may see the event after the gap, the client endpoint asks up to five other servers in the room, those with the most joined members first.
The first server that answers decides: if its event is closer, Zendrite backfills it with up to 20 events before it, so that `/context` and `/messages` can use it, and returns it when the user may see it.
Otherwise, or when no server answers, the local result stands.

### Faster joins

With `federation_api.serve_partial_state_joins: true`, Zendrite answers `PUT /_matrix/federation/v2/send_join/{roomId}/{eventId}` with `omit_members=true` (or the unstable `org.matrix.msc3706.partial_state=true`) as Synapse does:

- `state` holds the state before the join without `m.room.member` events, except the joining user's own membership and, when the room has neither an `m.room.name` nor an `m.room.canonical_alias`, the memberships of up to five heroes, so that the joining server's clients can name the room.
  Heroes are joined and invited members other than the joining user, oldest first; when there are none, members who left or were banned.
  Memberships that the join event cites as auth events are kept too, unlike in Synapse, so that a restricted join carries the membership of the user who authorized it (`join_authorised_via_users_server`): joining servers, Zendrite among them, check the join against the returned state.
- `auth_chain` holds the auth chain of that state and of the join event, without the events that are already in `state`.
- `members_omitted` is `true`, and `servers_in_room` lists the servers with a joined member before the join.

Without the parameter, on the v1 endpoint, and while the option is off (the default), the state is full.
The option is off by default because a Zendrite server that joins through this one then takes its own partial-state join path, described below, and Complement's federation tests between two Zendrite servers fail on it: sync timelines, device list updates, backfill and push rules of upgraded rooms all break until the resync is done.
While Zendrite itself only has partial state of a room, it cannot give a full answer, so `make_join`, `send_join`, `make_knock`, `send_knock` and `send_leave` for the room fail with 404 `M_NOT_FOUND`, as if Zendrite were not in the room.

Joining a remote room, Zendrite asks for partial state, stores which rooms have partial state and which servers are in them in the database, and resyncs the full state in the background from `/state_ids` at the room's latest event, fetching missing events from the join server and then the other servers.
The resync is queued again on startup for every room that still has partial state.
During the resync, federation `/state`, `/state_ids`, `/backfill`, `/event_auth` and `/get_missing_events` for the room fail with 403 `ORG.MATRIX.MSC3895_UNABLE_DUE_TO_PARTIAL_STATE`.
This differs from MSC3902 and Synapse, which hold `/members`, `/joined_members` and similar requests until the resync finishes, track the partial state of each event received during the resync, and re-check those events against the full state afterwards; most of Complement's `msc3902/TestPartialStateJoin` subtests fail for these reasons.

## Application services

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC3905](https://github.com/matrix-org/matrix-spec-proposals/pull/3905) | Application services should only be interested in local users | 1.5 | Implemented: `users` namespaces only match users on this server or its virtual hosts, so a remote sender, member or joined user no longer makes an event interesting to an application service |
| [MSC2409](https://github.com/matrix-org/matrix-spec-proposals/pull/2409) | Typing, receipts and presence for application services | 1.13 | Implemented: see [ephemeral data](#ephemeral-data-for-application-services) |
| [MSC2659](https://github.com/matrix-org/matrix-spec-proposals/pull/2659) | Application service ping endpoint | 1.7 | Implemented: `POST /_matrix/client/v1/appservice/{appserviceId}/ping`, authenticated with the application service's `as_token`, calls `POST /_matrix/app/v1/ping` with the `hs_token` and the `transaction_id`, and returns `duration_ms`. Failures return `M_URL_NOT_SET`, `M_CONNECTION_FAILED`, `M_CONNECTION_TIMEOUT` (after 30 seconds) or `M_BAD_STATUS` with the status and up to 4 KiB of the body |
| [MSC4326](https://github.com/matrix-org/matrix-spec-proposals/pull/4326) | Device masquerading for appservices | 1.17 | Implemented: see [devices of application services](#devices-of-application-services) |
| [MSC4190](https://github.com/matrix-org/matrix-spec-proposals/pull/4190) | Device management for application services | 1.17 | Implemented: see [devices of application services](#devices-of-application-services) |

### Ephemeral data for application services

An application service whose registration sets `receive_ephemeral: true` gets `m.typing`, `m.receipt` and `m.presence` in the `ephemeral` field of its transactions, in transactions of their own.
Older bridges that set the unstable `de.sorunome.msc2409.push_ephemeral: true` get the same data in `de.sorunome.msc2409.ephemeral`.
Typing notifications and receipts are sent for rooms the application service is interested in through its room or alias namespaces or a joined user in its user namespaces, with a `room_id`.
`m.typing` lists every user typing in the room, and a typing notification that times out sends an update without the user.
Private read receipts are only sent for users in the user namespaces.
Presence is sent for users in the user namespaces and for users who share a room with one of them.
Ephemeral data from before the application service enabled the option, or from while Zendrite was not running, is not sent.

### Devices of application services

An application service may add `device_id` to the `user_id` it asserts with its `as_token`, and then acts as that device of the user, for example when uploading one-time keys (MSC4326).
Without `user_id`, the device belongs to the application service's sender.
A device the user does not have fails with 400 `M_UNKNOWN_DEVICE`.
The unstable parameter `org.matrix.msc3202.device_id` is accepted too, and answered with `ORG.MATRIX.MSC4326.M_UNKNOWN_DEVICE`.
For any other access token, `device_id` is ignored.

Application services create devices for their users with `PUT /_matrix/client/v3/devices/{deviceId}`, which answers 201 when it created the device and 200 when it renamed an existing one (MSC4190).
The device gets an access token that is never handed out: the application service keeps using its `as_token` with `device_id`.
`DELETE /devices/{deviceId}`, `POST /delete_devices` and replacing cross-signing keys with `POST /keys/device_signing/upload` need no user-interactive authentication for application services.
With [delegated authentication](#msc3861-oidc-delegated-authentication), these device endpoints stay available to application services only, and `m.login.application_service` logins as well as application service registrations without `inhibit_login: true` fail with 400 `M_APPSERVICE_LOGIN_UNSUPPORTED`.
An `as_token` without `user_id` then acts as the sender, as it does without delegated authentication.

## Authentication

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC2265](https://github.com/matrix-org/matrix-spec-proposals/pull/2265) | Case folding of email addresses | 1.1 | Implemented: email 3PIDs are case-folded before they are stored, looked up or sent to an identity server. Addresses stored by older versions are folded on upgrade, except where the folded form is already in use, which is logged |
| [MSC2713](https://github.com/matrix-org/matrix-spec-proposals/pull/2713) | Remove deprecated identity service endpoints | 1.1 | Implemented: identity servers are only called through the authenticated v2 API, with the client's `id_access_token` or, for `email_validation_server`, a token the homeserver registers for its server notices user. 3PID invites require `id_access_token` and use hashed lookups |
| [MSC3288](https://github.com/matrix-org/matrix-spec-proposals/pull/3288) | Room type in `store-invite` | 1.2 | Implemented: 3PID invites send `room_type`, along with the room's name, avatar, join rule and alias |
| [MSC2858](https://github.com/matrix-org/matrix-spec-proposals/pull/2858) | Multiple SSO identity providers | 1.1 | Not applicable: SSO is only available through [MSC3861](#msc3861-oidc-delegated-authentication), where the authentication service offers the upstream providers, so the `m.login.sso` flow lists no `identity_providers` |
| [MSC2918](https://github.com/matrix-org/matrix-spec-proposals/pull/2918) | Refresh tokens | 1.3 | Implemented: clients sending `refresh_token: true` to `/login` or `/register` get a refresh token and an access token that expires after `client_api.refreshable_access_token_lifetime` (default `5m`, `0s` disables refresh tokens), which `/refresh` renews. An expired access token returns 401 `M_UNKNOWN_TOKEN` with `soft_logout: true`. A used refresh token stays valid until its successor is used, so a lost `/refresh` response can be retried. With [MSC3861](#msc3861-oidc-delegated-authentication), the authentication service issues the tokens |
| [MSC3882](https://github.com/matrix-org/matrix-spec-proposals/pull/3882) | Sign in a new session from an existing one | 1.7 | Implemented: `POST /_matrix/client/v1/login/get_token` asks for the password every time and returns a single-use `m.login.token` that expires after two minutes. `/login` lists the `m.login.token` flow with `get_login_token: true`, and `/capabilities` reports `m.get_login_token`. Redeeming a token deletes it first, so concurrent logins cannot share one. With [MSC3861](#msc3861-oidc-delegated-authentication), the endpoint returns 403 and the capability is disabled |
| [MSC4178](https://github.com/matrix-org/matrix-spec-proposals/pull/4178) | Error codes for `requestToken` | 1.13 | Implemented: `/account/3pid/email/requestToken` (and `/register/email/requestToken`) return 400 `M_THREEPID_MEDIUM_NOT_SUPPORTED` without an email validation server and `M_INVALID_PARAM` for an invalid address; the `msisdn/requestToken` endpoints always return `M_THREEPID_MEDIUM_NOT_SUPPORTED` |
| [MSC3861](https://github.com/matrix-org/matrix-spec-proposals/pull/3861) | Next-gen auth (OIDC) | 1.15 | Opt-in (`msc3861`) |
| [MSC4312](https://github.com/matrix-org/matrix-spec-proposals/pull/4312) | `m.oauth` user-interactive authentication | 1.17 | Implemented with [MSC3861](#msc3861-oidc-delegated-authentication): replacing cross-signing keys asks for confirmation through the provider's account management page |
| [MSC4191](https://github.com/matrix-org/matrix-spec-proposals/pull/4191), [MSC3824](https://github.com/matrix-org/matrix-spec-proposals/pull/3824), [MSC4341](https://github.com/matrix-org/matrix-spec-proposals/pull/4341) | OAuth 2.0 account management, OAuth-aware clients, device authorization grant | 1.18 | Implemented with [MSC3861](#msc3861-oidc-delegated-authentication): `/auth_metadata` passes on the provider's account management URL, actions and grant types, and `GET /login` marks SSO as `oauth_aware_preferred` |
| [MSC4108](https://github.com/matrix-org/matrix-spec-proposals/pull/4108), [MSC4388](https://github.com/matrix-org/matrix-spec-proposals/pull/4388) | QR code login and its rendezvous sessions | N/A | Implemented with [MSC3861](#msc3861-oidc-delegated-authentication): the MSC4388 rendezvous API and the 2024 MSC4108 rendezvous API that Element X still uses. See [QR code login](#qr-code-login) |

### QR code login

[MSC4108](https://github.com/matrix-org/matrix-spec-proposals/pull/4108) signs in a new device by scanning a QR code shown on a device that is already signed in, or the other way round.
The two devices exchange messages through a rendezvous session on the homeserver, encrypted end to end, and the new device then signs in with the OAuth 2.0 device authorization grant of [MSC3861](#msc3861-oidc-delegated-authentication).
Zendrite only provides the rendezvous sessions; the provider handles the sign-in.
Both rendezvous APIs below are on by default when MSC3861 is enabled, and unavailable without it.

The current MSC4108 text moves the rendezvous session to [MSC4388](https://github.com/matrix-org/matrix-spec-proposals/pull/4388), served under `/_matrix/client/unstable/io.element.msc4388/rendezvous`:

- `GET /rendezvous` reports `create_available`: whether the requester may create a session.
- `POST /rendezvous` creates a session with an initial `data` payload and returns its `id`, `sequence_token` and `expires_in_ms`.
- `GET /rendezvous/{id}` returns the payload, its `sequence_token` and `expires_in_ms`, without an access token.
  A request that a browser makes to navigate to the URL (`Sec-Fetch-Dest` other than `empty`, `Sec-Fetch-Mode: navigate`, `Sec-Fetch-User: ?1` or `Sec-Fetch-Site: none`) is refused with 403 `M_FORBIDDEN`, so a session cannot serve content to someone who opens its link.
- `PUT /rendezvous/{id}/{txnId}` replaces the payload if `sequence_token` is still the current one, and otherwise fails with 409 `IO_ELEMENT_MSC4388_CONCURRENT_WRITE`.
  The `sequence_token` of a successful write is recorded against its transaction ID for the session's lifetime: a retry with that transaction ID gets the same answer and writes nothing, whatever its body.
  A session remembers its last 100 transaction IDs.
- `DELETE /rendezvous/{id}` ends a session.

Payloads are limited to 4096 bytes (413 `M_TOO_LARGE`), and sequence tokens count the writes of a session, starting at `1`.
Only users with an access token can create MSC4388 sessions by default; guests cannot.
`msc4388_mode: open` lets anyone create them, which allows a new device that has not chosen its homeserver yet to use this one.
Even then, a request with an invalid access token fails with 401, as MSC4388 requires.

Current Element X and matrix-rust-sdk releases still create their sessions through the 2024 version of MSC4108, under `/_matrix/client/unstable/org.matrix.msc4108/rendezvous`, and Element X only offers QR code login when `/versions` advertises `org.matrix.msc4108`.
Zendrite serves it alongside MSC4388, as Synapse does.
`POST` creates a session from a `text/plain` body and returns its `url`; `GET`, `PUT` and `DELETE` on that URL read, replace and end it, with the `ETag` as the sequence (`If-None-Match` for 304, `If-Match` for the compare-and-swap, 412 on a stale ETag).
Its clients create sessions without an access token, so this API is always open; turn it off with `msc4108_enabled: false`.
The session URL is built from `mscs.msc3861.public_base_url`, else `global.well_known_client_name`, else the request's host.
MSC4388 has no `/versions` flag; clients discover it through `GET /rendezvous`.

Sessions are stored in the user API database, so every replica of an HA deployment serves them, and a write is a compare-and-swap on the database row.
They expire `session_lifetime` (default three minutes, between two and five allowed) after their creation, and expired sessions are deleted every five minutes.
At most `max_sessions` (default 1000) sessions created without an access token, or by guest and suspended accounts in open mode, may be active at once; further such creations get 429.
Other users do not count towards it, so anonymous callers cannot lock them out of QR code login; each of them may have ten active sessions instead.
Every rendezvous request is rate limited per address by `rate_limiting` (default a burst of 20, then five per second), before any access token is checked.
Creation is additionally limited by `create_rate_limiting` (default five per minute), per device for requests with an access token and per address otherwise.
Both devices poll the session about once a second, possibly from the same address.

```yaml
mscs:
  mscs:
    - msc3861
  rendezvous:
    # off, authenticated (default) or open
    msc4388_mode: authenticated
    msc4108_enabled: true
    session_lifetime: 3m
    # active sessions created without an access token
    max_sessions: 1000
```

## VoIP

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC4140](https://github.com/matrix-org/matrix-spec-proposals/pull/4140) | Cancellable delayed events | N/A (accepted) | Implemented with stable and unstable endpoints, disabled by default: see [delayed events](#delayed-events) |
| [MSC4143](https://github.com/matrix-org/matrix-spec-proposals/pull/4143) | MatrixRTC transport discovery | N/A | Implemented (configure via [`rtc_foci`](/administration/matrixrtc/)) |

### Delayed events

[MSC4140](https://github.com/matrix-org/matrix-spec-proposals/pull/4140) lets a client schedule an event for the server to send later, unless the client restarts or cancels the schedule first.
Element Call uses it to end the call membership of a client that crashes or loses its connection; see [MatrixRTC](/administration/matrixrtc/#delayed-events).
The MSC has been accepted but is not part of a spec release yet.
Zendrite implements its stable endpoints, which `org.matrix.msc4140.stable` in `/versions` announces:

- `PUT /_matrix/client/v3/rooms/{roomId}/delayed_event/{eventType}/{txnId}` schedules an event from a body of `delay_ms`, `content` and an optional `state_key`, and returns `{"delay_id": ...}`.
- `POST /_matrix/client/v1/delayed_events/{delayId}/{cancel|restart|send}` manages one of the user's scheduled events.
- `GET /_matrix/client/v1/delayed_events/{delayId}` returns one of the user's delayed events, including a `finalised` object once it was sent, canceled or failed.

All three need an access token and are open to guests.
There is no stable list endpoint, which the MSC leaves to [MSC4486](https://github.com/matrix-org/matrix-spec-proposals/pull/4486).

Zendrite also keeps the unstable API that matrix-js-sdk and Synapse use:

- The `org.matrix.msc4140.delay` query parameter, in milliseconds, on `PUT /_matrix/client/v3/rooms/{roomId}/send/{eventType}/{txnId}` and `PUT /_matrix/client/v3/rooms/{roomId}/state/{eventType}/{stateKey}` schedules the event and returns `{"delay_id": ...}` instead of an event ID.
- `PUT /_matrix/client/unstable/org.matrix.msc4140/rooms/{roomId}/delayed_event/{eventType}/{txnId}` schedules an event from a body of `delay_ms`, `content` and an optional `state_key`.
- `POST /_matrix/client/unstable/org.matrix.msc4140/delayed_events/{delayId}/{cancel|restart|send}`, and the older form `POST .../delayed_events/{delayId}` with `{"action": ...}`, manage a scheduled event.
  As in Synapse, these work without an access token, since the delay ID identifies the event; with one, they are rate-limited per device instead of per address.
- `GET /_matrix/client/unstable/org.matrix.msc4140/delayed_events/{delayId}` returns one of the user's delayed events, including a `finalised` object once it was sent, canceled or failed.
- `GET /_matrix/client/unstable/org.matrix.msc4140/delayed_events` lists the user's scheduled events, or with `status=finalised` the finalized ones, optionally only those named by `delay_id` parameters, with the field names of earlier revisions (`delay`, `running_since`) included as Synapse does.
  The list is not paginated, so `from` is ignored.

When scheduling, the event is built against the room's current state, so a malformed event, or one the user may not send, is refused at once.
A delay must be a positive integer; one above the maximum fails with 400 `M_DELAY_TOO_LARGE` on the stable endpoint and `ORG.MATRIX.MSC4140_DELAY_TOO_LARGE` on the unstable ones, and a user with as many events scheduled as allowed gets 429 `M_LIMIT_EXCEEDED` with `Retry-After` set to when their next event is due.
When delayed events are disabled, scheduling fails with 403 `M_FORBIDDEN`.

At its send time, the event is built from the room state at that moment and sent through the same path as a client's send, including the authorization checks.
A failure is recorded as the event's `finalised.error` and is not retried.
The `send` action differs: if sending fails, it returns the error and keeps the event scheduled.
The sent event carries its delay ID as both `delay_id` and `org.matrix.msc4140.delay_id` in its `unsigned` data in the sender's `/sync`, and no `transaction_id`.
A message event scheduled with `org.matrix.msc4354.sticky_duration_ms`, through either scheduling endpoint, is sent as a [sticky event](#sticky-events), whose sticky period starts when it is sent.
As for a direct send, the parameter is ignored for state events.

Retrying an action is safe: `send` succeeds for an event that was already sent, and `cancel` for one that was already canceled or failed.
On the stable endpoints, an action that conflicts with how the event was finalized, such as restarting one that was sent, returns 409, and another user's delay ID returns 404 `M_NOT_FOUND`.
The unstable endpoints return 404 `M_NOT_FOUND` for a conflict, as Synapse does, because older matrix-js-sdk versions schedule a new delayed leave event only when a restart finds none.

A user's scheduled state event is canceled when another user changes the same state, and all of a user's delayed events are deleted when the account is deactivated.
Finalized events are kept for seven days, and at most 1000 per user.

Delayed events are stored in the user API database and sent by the process that serves clients.
In HA deployments, that is the active process: a successor picks up the schedule from the database, and resends any event its predecessor was sending when it stopped.
The resend uses the same roomserver transaction, keyed by the delay ID, so an event that did reach the room is not sent twice.

`/versions` advertises `org.matrix.msc4140` and `org.matrix.msc4140.stable` while delayed events are enabled, and `/capabilities` reports the limits as both `m.delayed_events` and `org.matrix.msc4140.delayed_events`, where a `max_delay_ms` of 0 means disabled.

### Poll push rules

Zendrite installs the MSC3930 default push rules with the unstable rule IDs and [MSC3381](https://github.com/matrix-org/matrix-spec-proposals/pull/3381) event types that Synapse and current clients use:

- `.org.matrix.msc3930.rule.poll_response`, the last default `override` rule, keeps `org.matrix.msc3381.poll.response` events from notifying.
- `.org.matrix.msc3930.rule.poll_start_one_to_one` and `.org.matrix.msc3930.rule.poll_end_one_to_one` notify with the default sound for `org.matrix.msc3381.poll.start` and `org.matrix.msc3381.poll.end` in rooms with two members.
- `.org.matrix.msc3930.rule.poll_start` and `.org.matrix.msc3930.rule.poll_end` notify for them in other rooms.

The four `underride` rules come after the other default `underride` rules, each one-to-one rule before its counterpart.
Polls in encrypted rooms are `m.room.encrypted` events, which the encryption rules handle.
Existing accounts receive the rules when their push rules are next read; a rule the user disabled stays disabled.
Unlike Synapse, where they need `msc3381_polls_enabled`, the rules are always installed, because MSC3930 is accepted.

## Client-only MSCs

Some spec MSCs need no server support beyond storing events, account data, media or to-device messages, so they work with Zendrite as they are:
[MSC4142](https://github.com/matrix-org/matrix-spec-proposals/pull/4142) (no intentional mentions from replies, 1.16),
[MSC4313](https://github.com/matrix-org/matrix-spec-proposals/pull/4313) and [MSC4230](https://github.com/matrix-org/matrix-spec-proposals/pull/4230) (rendering, 1.18),
[MSC4153](https://github.com/matrix-org/matrix-spec-proposals/pull/4153) (excluding devices that are not cross-signed, 1.18),
[MSC4356](https://github.com/matrix-org/matrix-spec-proposals/pull/4356) (recent emoji, 1.18),
[MSC2545](https://github.com/matrix-org/matrix-spec-proposals/pull/2545) (image packs, 1.19),
[MSC4287](https://github.com/matrix-org/matrix-spec-proposals/pull/4287) (key backup preference, 1.19)
and [MSC4268](https://github.com/matrix-org/matrix-spec-proposals/pull/4268) (encrypted history sharing, 1.19).

## Opt-in MSCs

The following MSCs are not enabled by default and must be activated in the `mscs` section of the config file:

```yaml
mscs:
  mscs:
    - msc2836
    - msc2444
    - msc2753
    - msc3814
    - msc3861
    - msc4028
```

### MSC2836: Threading

The relationship index behind `POST /_matrix/client/unstable/event_relationships` is built from the roomserver output stream by the durable JetStream consumer `MSC2836RoomServerConsumer`.
A relationship is therefore queryable shortly after its event is sent, not necessarily by the time the send request returns.
A failed index write is retried until it succeeds, including by an HA successor, so a stored event never permanently loses its relationship.
Disabling the MSC removes the consumer at the next start, except in HA mode, where it must be removed during the offline upgrade.

### MSC2444 and MSC2753: Peeking

Peeking into a room whose ID names no server, as in room version 12, works locally when this server is in the room.
Any other such room needs `?server_name=` on `POST /_matrix/client/v3/peek/{roomIdOrAlias}` to peek over federation.
Only world-readable, unencrypted rooms can be peeked, locally or by a remote server.
Remote peeks ask to be renewed hourly; peeking again after the interval renews an expired outbound peek.

### MSC3814: Dehydrated Devices v2

MSC3814 allows clients to store a "dehydrated" device on the server so that encrypted to-device messages (e.g. key shares) can be delivered while the user is offline.
When the user signs in again, the client rehydrates the device and retrieves messages it missed.

**What changes when MSC3814 is enabled:**

- `PUT /_matrix/client/unstable/org.matrix.msc3814.v1/dehydrated_device` — store a dehydrated device with its keys.
- `GET /_matrix/client/unstable/org.matrix.msc3814.v1/dehydrated_device` — retrieve the current dehydrated device metadata.
- `DELETE /_matrix/client/unstable/org.matrix.msc3814.v1/dehydrated_device` — remove the dehydrated device.
- `POST /_matrix/client/unstable/org.matrix.msc3814.v1/dehydrated_device/{deviceID}/events` — retrieve to-device events addressed to the dehydrated device.

**Configuration:**

```yaml
mscs:
  mscs:
    - msc3814
```

No additional configuration is needed beyond enabling the MSC.

### MSC4028: Push all encrypted events

MSC4028 lets clients that decrypt notifications themselves, such as Element X, receive a push for every encrypted event and decide after decrypting it whether to show it.
It is off by default, as in Synapse, where it needs `msc4028_push_encrypted_events`.

**What changes when MSC4028 is enabled:**

- `/versions` advertises `org.matrix.msc4028` in `unstable_features`.
- The default `override` rule `.org.matrix.msc4028.encrypted_event` (Synapse's unstable ID; the MSC calls it `.m.rule.encrypted_event`) notifies for every `m.room.encrypted` event. As in Synapse, it is enabled and comes right after `.m.rule.master`.
- Rules the user added take precedence, so muted rooms stay muted, but encrypted events in a room set to mentions and keywords only notify, and count as unread notifications.
- New accounts get the rule, and existing accounts receive it when their push rules are next read. Users can disable it like any default rule.

Removing `msc4028` from the configuration removes the rule from every account when its push rules are next read, including a user's choice to disable it; enabling the MSC again adds it back enabled.

```yaml
mscs:
  mscs:
    - msc4028
```

### MSC3861: OIDC Delegated Authentication

MSC3861 delegates authentication to an external OpenID Connect (OIDC) provider such as [Matrix Authentication Service (MAS)](https://github.com/element-hq/matrix-authentication-service).
When enabled, Zendrite validates access tokens via OAuth 2.0 token introspection instead of managing passwords directly, and provisions accounts and devices on demand.

This changes the behaviour of a large part of the client-server API: password login and registration are disabled, session management moves to the provider, and clients without native OIDC support go through a legacy SSO compatibility layer.

**Configuration:**

```yaml
mscs:
  mscs:
    - msc3861
  msc3861:
    issuer: "https://auth.example.com/"
    client_id: "0000000000000000000ZENDRITE"
    client_secret: "secret"
```

See [Delegated authentication (OIDC)](/administration/oidc/) for the full setup guide, the complete configuration reference, client support, account provisioning rules, migration caveats and troubleshooting.
It is qualified for active-passive HA; [High availability](/administration/oidc/#high-availability) there lists what a takeover loses and how clients recover.
