---
title: Supported admin APIs
description: Administrative API endpoints for Zendrite
---

Zendrite supports a small number of admin endpoints.
They have no API stability guarantee and may change shape without warning.

Call them with curl:

```bash
curl --header "Authorization: Bearer <access_token>" -X <POST|GET|PUT> <Endpoint URI> -d '<Request Body Contents>'
```

Most Element-based Matrix clients show the `access_token` under `Settings` -> `Help & About` -> `Advanced` -> `Access Token`.
Keep the `access_token` **secret**, because anyone who has it can act as that user.

Only users marked as administrators in the `userapi_accounts` table can call these endpoints.
To make an existing account an administrator, set its `account_type` to `3` in `userapi_accounts`:

```sql
UPDATE userapi_accounts SET account_type = 3 WHERE localpart = '$localpart';
```

`$localpart` is the username only, for example `alice`.

## POST `/_zendrite/admin/evacuateRoom/{roomID}`

Zendrite removes all local users from the room given by `roomID` in the URL.
This can take some time.
The response is a JSON body with the user IDs of all affected users.

If the room has an alias (for example, because it's published), Element Web shows the alias in its URL instead of the room ID.
The room ID is the room's "internal ID" under **Settings > Advanced** in Element Web.

## POST `/_zendrite/admin/evacuateUser/{userID}`

Zendrite removes the local user given by `userID` in the URL from all rooms they have joined.
The response is a JSON body with the room IDs of all affected rooms.

## POST `/_zendrite/admin/resetPassword/{userID}`

Resets the password of a local user.

**If `logout_devices` is `true`, Zendrite invalidates all `access_tokens`, and the user may lose access to encrypted messages.**

Request body format:

```json
{
  "password": "new_password_here",
  "logout_devices": false
}
```

## GET `/_zendrite/admin/fulltext/reindex`

Zendrite reindexes all searchable events (`m.room.message`, `m.room.topic`, and `m.room.name`).
It returns an empty JSON body immediately and indexes in the background.
The server logs progress for every batch of up to 1000 events.
When reindexing finishes, your debug logs show a line such as `Indexed 69586 events in 53.68223182s`.

## POST `/_zendrite/admin/refreshDevices/{userID}`

Zendrite immediately queries `/devices/{userID}` on the user's federated server and updates all locally stored devices and keys for that user.
It returns an empty JSON body on success.
Use it to try to fix E2EE problems where the remote user can't decrypt messages.

## POST `/_zendrite/admin/purgeRoom/{roomID}`

Zendrite removes the room from its database.
First, it tries to evacuate all local users from the room.
It does **not** remove media files.
Large rooms can take a while.
The endpoint returns an empty JSON body once Zendrite has told the other components to delete the room.

Zendrite can also purge empty rooms automatically, see [Purge empty rooms automatically](/administration/auto-purge-empty-rooms/).

## GET `/_zendrite/admin/emptyRooms`

Returns all rooms with zero locally joined members.
Response format:

```json
{
  "empty_rooms": ["!roomid1:server_name", "!roomid2:server_name"]
}
```

## POST `/_synapse/admin/v1/send_server_notice`

Sends a server notice to one user.
Request body format:

```json
{
  "user_id": "@target_user:server_name",
  "content": {
    "msgtype": "m.text",
    "body": "This is my message"
  }
}
```

See the [Matrix Spec](https://spec.matrix.org/v1.3/client-server-api/#server-notices) for how server notices behave.
On success, the API returns:

```json
{
  "event_id": "<event_id>"
}
```

## GET `/_synapse/admin/v1/register`

Shared secret registration.
See [Creating user accounts](/administration/createusers) to configure and use this endpoint.

## GET and PUT `/_matrix/client/v1/admin/lock/{userId}`

Defined in the [Matrix Spec](https://spec.matrix.org/v1.18/client-server-api/#put_matrixclientv1adminlockuserid).

- `PUT` with `{"locked": true}` locks a local account, and `{"locked": false}` removes the lock.
- `GET` returns `{"locked": <bool>}`.

A locked account keeps its sessions, but can only log out until an admin removes the lock.
See [account locking](/mscs/#account-locking).

Errors:

- 400 `M_INVALID_PARAM` if the user ID doesn't belong to this server.
- 404 `M_NOT_FOUND` if the account doesn't exist or is deactivated.
- 403 `M_FORBIDDEN` if an administrator tries to lock themselves or another administrator.
- 403 for both methods with MSC3861 delegated authentication.
  Lock the account in the authentication service instead.

## GET and PUT `/_matrix/client/v1/admin/suspend/{userId}`

Defined in the [Matrix Spec](https://spec.matrix.org/v1.18/client-server-api/#put_matrixclientv1adminsuspenduserid).

- `PUT` with `{"suspended": true}` suspends a local account, and `{"suspended": false}` lifts the suspension.
- `GET` returns `{"suspended": <bool>}`.

A suspended account can still log in and read, but can't join rooms, send messages, or change its profile.
See [account suspension](/mscs/#account-suspension).

The errors match the lock endpoints.
Suspension also works with MSC3861 delegated authentication.

## GET `/_synapse/admin/v1/room_reports`

Lists room reports (MSC4151), newest first, like Synapse.
Filter the list with these optional parameters:

- `from`: offset, default 0.
- `limit`: default 100.
- `dir`: `b` (the default) or `f` for oldest first.
- `user_id`: the reporter.
- `room_id`: the reported room.

The response contains `room_reports`, `total`, and `next_token` when more reports follow.
Each report has `id`, `received_ts`, `room_id`, `name`, `canonical_alias`, `user_id`, and `reason`.

## GET `/_synapse/admin/v1/user_reports`

Lists user reports (MSC4260), newest first, in the same shape as room reports.
Filter the list with these optional parameters:

- `from`: offset, default 0.
- `limit`: default 100.
- `dir`: `b` (the default) or `f` for oldest first.
- `user_id`: the reporter.
- `target_user_id`: the reported user.

The response contains `user_reports`, `total`, and `next_token` when more reports follow.
Each report has `id`, `received_ts`, `target_user_id`, `user_id`, and `reason`.

## GET `/_matrix/client/v3/admin/whois/{userId}`

Defined in the [Matrix Spec](https://spec.matrix.org/v1.3/client-server-api/#get_matrixclientv3adminwhoisuserid).
Returns information about a user.
`userId` is the full user ID, for example `@alice:domain.com`.
