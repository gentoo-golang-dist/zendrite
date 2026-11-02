---
title: Client API conformance
description: Version discovery policy, audited implementation gaps, and experimental feature flags.
---

## Advertisement decision

`GET /_matrix/client/versions` advertises `r0.0.1`, `r0.1.0`, `r0.2.0`, `r0.3.0`, `r0.4.0`, `r0.5.0`, `r0.6.1`, and `v1.0` to `v1.19`.
The versions up to `v1.2` are a compatibility baseline, not a new certification of every historical requirement.
`v1.3` to `v1.19` are advertised because the audit of every MSC merged into the specification since v1.1 ([issue #478](https://codefloe.com/pat-s/zendrite/issues/478)) leaves no unresolved requirement in those releases; the sections below record the evidence.
Open MSCs that clients use but that are not part of a spec release are tracked in [issue #558](https://codefloe.com/pat-s/zendrite/issues/558); the ones Zendrite implements are listed under [experimental feature discovery](#experimental-feature-discovery).

The [version discovery contract](https://spec.matrix.org/v1.19/client-server-api/#api-versions) distinguishes specification versions from individual experimental features.
Authenticated media, room summaries, and selected other newer features are implemented, but their presence does not establish support for the other requirements introduced in the same release.
Room versions returned by `/capabilities` are a separate namespace and must not be copied into `/versions`.

## Scope and evidence

This is a source-level audit of v1.3 through v1.19, the versions advertised beyond the compatibility baseline.
It compares the release requirements with routing, handler behavior, storage, and existing tests in this repository.
It does not claim an exhaustive behavioral certification or a fresh Sytest/Complement pass.
A registered route establishes reachability only; authorization, pagination, response shape, and configuration dependence require separate evidence.
An unresolved mandatory requirement is sufficient to withhold a version advertisement.
Optional features and client-only presentation changes are not automatically blockers.

Source paths below are relative to the repository root.

### v1.3

The [v1.3 changes](https://spec.matrix.org/v1.3/changelog/) introduce optional `from` on messages, refresh tokens, relationship retrieval and aggregation rules, relaxed replies, and `knock_restricted` rooms.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Messages without `from` | `syncapi/routing/messages.go` constructs earliest/latest topology positions according to direction. | Implemented path; this alone does not certify pagination. |
| Refresh tokens | `clientapi/routing/refresh.go` serves `/v3/refresh` for refresh tokens issued by `/login` and `/register` for clients that request them; with MSC3861, `msc3861_sso_compat.go` delegates refresh to the provider. `clientapi/refresh_token_test.go` covers rotation, retrying a lost response, expiry and logout. | Implemented; access tokens of clients that request a refresh token expire after `client_api.refreshable_access_token_lifetime` (default five minutes), and other clients keep non-expiring tokens. |
| Relationship retrieval | `syncapi/routing/relations.go` serves all three `/v1/rooms/{roomID}/relations/...` forms in topological order; `syncapi/routing/relations_test.go` covers them. | Implemented; see below. |
| Aggregation framework and relaxed replies | `syncapi/internal/aggregations.go` calculates bundled relations for the endpoints the spec lists, including limited `/sync` timelines. | v1.3 defines no relation type requiring aggregation; the framework is covered by `syncapi/aggregations_test.go`. |
| `knock_restricted` | Room-version handling uses gomatrixserverlib; `roomserver/roomserver_test.go` exercises restricted and knock-restricted joins with room version 10. | Implementation and focused tests exist; not evidence for unrelated client APIs. |

The [v1.3 relations endpoint](https://spec.matrix.org/v1.3/client-server-api/#get_matrixclientv1roomsroomidrelationseventid) accepts pagination tokens from `/messages` and `/sync`, returns children in topological order, and specifies 404 for a missing or inaccessible parent.
`syncapi/routing/relations.go` accepts its own tokens, `/messages` topology tokens and `/sync` stream tokens for `from` and `to`, with the same cursor semantics as `/messages`.
The storage query orders children by event topology, so a backfilled child sorts by its depth rather than its arrival.
A missing parent, one in another room and one hidden by history visibility all return 404 `M_NOT_FOUND`.
Children hidden by history visibility or sent by ignored users are left out, and each child carries its own bundled aggregations.
These resolve the relations gaps that blocked v1.3.
Timestamp massaging (MSC3316) is limited to application services, and v1.3 is advertised.

### v1.4 and v1.5

The [v1.4 changes](https://spec.matrix.org/v1.4/changelog/) add edits, threads, private and threaded receipts, room-type directory filters, room-type preservation on upgrade, a server-ACL push rule, and media CORP headers.
The v1.3 relations behavior is covered above.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Private receipts | `clientapi/routing/receipt.go` accepts `m.read.private`; `readmarker.go` accepts optional markers; `syncapi/streams/stream_receipt.go` excludes another user's private receipts. | Retain the existing MSC2285 compatibility flag. |
| Thread listing | `syncapi/routing/threads.go` serves `/v1/rooms/{roomID}/threads` with `include` filtering and pagination. | Implemented; covered by `threads_test.go`. |
| Threaded receipts and notification counts | `clientapi/routing/receipt.go` validates `thread_id`, receipts keep their thread identity, and `syncapi/streams/stream_notificationdata.go` serves per-thread counts when the filter sets `unread_thread_notifications`. | Implemented; covered by `TestThreadNotificationCounts` and `TestThreadReplyCountedInThread`. |
| Edit and thread bundles | `syncapi/internal/aggregations.go` bundles the latest valid edit under `m.replace` and the thread summary under `m.thread`, filtered for history visibility and ignored users. | Implemented. |
| Room-type directory filters | `clientapi/routing/directory_public.go` and `federationapi/routing/publicrooms.go` filter with `roomserver/api.FilterPublicRooms` and return `room_type`; remote client results are filtered again. | Implemented; covered by `TestPublicRoomsRoomTypes`, `TestPublicRoomsRemoteRoomTypes` and `TestFederationPublicRoomsRoomTypes`. |
| Room upgrades | `roomserver/internal/perform/perform_upgrade.go` copies the old create event's content, including `type`, into the new create event. | Implemented. |
| Server ACL push rule | `internal/pushrules/default_override.go` installs `.m.rule.room.server_acl`. | Implemented. |
| Media CORP header | `mediaapi/routing/routing.go` sends `Cross-Origin-Resource-Policy: cross-origin`. | Implemented. |

[v1.5](https://spec.matrix.org/v1.5/changelog/) adds reference relations and documents in-room verification requests.
References are bundled under `m.reference` and can be paginated through `/relations`.
Application service `users` namespaces only match local users (MSC3905).
Both v1.4 and v1.5 are advertised.
MSC2836's experimental relationship API is not the stable `m.thread` protocol or thread-list endpoint.

### v1.6

The [v1.6 changes](https://spec.matrix.org/v1.6/changelog/) add jump to date, remove the `keyId` parameters from the key endpoints, and extend `send_join` for faster joins.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Jump to date (MSC3030) | `syncapi/routing/timestamp_to_event.go` serves `/v1/rooms/{roomID}/timestamp_to_event` and the federation `/v1/timestamp_to_event/{roomID}`. When the local history has a gap that may hold a closer event, the client endpoint asks other servers in the room and backfills their answer. `timestamp_to_event_test.go` covers both endpoints and the remote lookup. | Implemented. |
| Key queries without `keyId` (MSC3938) | `federationapi/routing/routing.go` serves `GET /_matrix/key/v2/query/{serverName}`; `TestNotaryServer` covers it. | Implemented. |
| Faster joins (MSC3706, MSC3943) | `federationapi/internal/federationclient.go` joins with `omit_members=true`. Serving `send_join`, Zendrite always returns the full state with `members_omitted` unset, which the spec allows. | Compliant. |

### v1.7

The [v1.7 changes](https://spec.matrix.org/v1.7/changelog/) add asynchronous media uploads, media download redirects, the application service ping, reactions, signing in from an existing session, and scope transaction IDs to devices.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Asynchronous uploads (MSC2246) | `mediaapi/routing/async_upload.go` serves `POST /v1/create` and `PUT /upload/{serverName}/{mediaId}`, and makes downloads of pending media wait for `timeout_ms` before 504 `M_NOT_YET_UPLOADED`. `async_upload_test.go` covers the errors, concurrent uploads, waiting across replicas and the pending limit. | Implemented. |
| Media redirects (MSC3860) | Zendrite never redirects, which the spec allows. `mediaapi/routing/media_redirect.go` follows a `Location` in a remote federation media response; `media_redirect_test.go` covers it. | Compliant. |
| Application service ping (MSC2659) | `clientapi/routing/appservice_ping.go` serves `POST /v1/appservice/{appserviceId}/ping`; `appservice_ping_test.go` covers every error code. | Implemented. |
| Duplicate annotations (MSC2677) | `roomserver_annotations` indexes unredacted annotations, and `validateAnnotation` in `clientapi/routing/sendevent.go` returns 400 `M_DUPLICATE_ANNOTATION`; `clientapi/annotations_test.go` covers it. | Implemented. |
| Login tokens (MSC3882) | `clientapi/routing/login_token.go` serves `POST /v1/login/get_token` behind password re-authentication, with the `m.get_login_token` capability and `get_login_token` on the `m.login.token` flow; `login_token_test.go` covers single use. | Implemented; disabled with MSC3861, which the spec allows. |
| Transaction IDs per device (MSC3970) | `internal/transactions` keys its cache by device, or by application service, and endpoint. | Implemented. |

### v1.8

The [v1.8 changes](https://spec.matrix.org/v1.8/changelog/) add room version 11, `matrix-fed` SRV discovery, `+` in user IDs, and require reporters of an event to be joined to its room.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Room version 11 (MSC3820, MSC2174, MSC2175, MSC2176, MSC3821, MSC3989) | gomatrixserverlib implements the version; the roomserver applies room v11 redactions from `content.redacts` and keeps `third_party_invite.signed` when redacting membership events. | Implemented. |
| `matrix-fed` SRV records (MSC4040) | gomatrixserverlib's `fclient/resolve.go` looks up `_matrix-fed._tcp` before the deprecated `_matrix._tcp`. | Implemented. |
| `+` in user IDs (MSC4009) | `internal/validate.go` accepts `+` in localparts. | Implemented. |
| Reporting events (MSC2249) | `clientapi/routing/report_event.go` returns 404 unless the reporter is joined to the room. | Implemented. |

### v1.9

[v1.9](https://spec.matrix.org/v1.9/changelog/) adds the `.m.rule.suppress_edits` default push rule (MSC3958), which `internal/pushrules/default_override.go` installs; the other changes are clarifications.

### v1.10

The [v1.10 changes](https://spec.matrix.org/v1.10/changelog/) add support discovery, local erasure requests, recursive relations, optional authentication on `/versions`, and `Retry-After` on rate limits, and reserve `m.push_rules` in the account data API.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Support discovery (MSC1929) | `clientapi/routing/routing.go` serves `/.well-known/matrix/support` from `global.support`, or 404 when unset; `wellknown_support_test.go` and `config_support_test.go` cover it. | Implemented. |
| Erasure requests (MSC4025) | `/account/deactivate` and the MAS `delete_user` call record the user in `roomserver_erased_users`. `ApplyHistoryVisibilityFilter` and the federation `/event`, `/backfill` and `/get_missing_events` paths serve their events redacted to viewers without membership at the event; account data and 3PIDs are deleted. `clientapi/erasure_test.go`, `roomserver/erasure_test.go` and `history_visibility_erasure_test.go` cover it. | Implemented. |
| Push rules in account data (MSC4010) | `clientapi/routing/account_data.go` rejects `m.push_rules` and `m.fully_read` with 405 `M_BAD_JSON`; `TestServerManagedAccountData` covers it. | Implemented. |
| Recursive relations (MSC3981) | `syncapi/routing/relations.go` parses `recurse`, follows relations three levels deep and reports `recursion_depth`. | Implemented. |
| Optional authentication on `/versions` (MSC4026) | `/versions` ignores credentials; `versions_test.go` covers unauthenticated and invalid bearer requests. | Compliant. |
| `Retry-After` (MSC4041) | `internal/httputil/rate_limiting.go` sets `Retry-After` next to `retry_after_ms`. | Implemented. |
| Application service login without a valid `as_token` | `clientapi/auth/login_application_service.go` requires an application service token. | Implemented. |

### v1.11

The [v1.11 changes](https://spec.matrix.org/v1.11/changelog/) add authenticated media with URL previews, animated thumbnails, terms of service at registration, membership metadata on events, and cross-signing uploads without UIA.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Authenticated media (MSC3916) | `mediaapi/routing/routing.go` serves the `/_matrix/client/v1/media/*` and `/_matrix/federation/v1/media/*` routes; `mediaapi/routing/url_preview.go` serves `preview_url` when `media_api.url_preview.enabled` is set, and `url_preview_test.go` covers OpenGraph parsing, images, caching and address blocking. | Implemented; previews are opt-in, as in Synapse. |
| Animated thumbnails (MSC2705) | `mediaapi/routing/download.go` never answers a thumbnail request without `animated=true` with an animated image; `animated_thumbnail_test.go` and `thumbnailer/animated_test.go` cover GIF, APNG and WebP. | Implemented. |
| Terms of service (MSC1692) | `client_api.registration_terms` adds `m.login.terms` to every registration flow, and `auth_fallback.go` serves its fallback page; `register_terms_test.go` covers both. | Implemented; optional. |
| Membership on events (MSC4115) | `ApplyHistoryVisibilityFilter` records the requesting user's membership at each event, which `synctypes.ToClientEvent` serves as `unsigned.membership`; `membership_annotation_test.go` covers `/sync`, `/messages` and `/event`. | Implemented. |
| Cross-signing without UIA (MSC3967) | `clientapi/routing/key_crosssigning.go` only asks for UIA when a master key exists and the upload changes the keys. | Implemented. |
| Optional access token on `/account/password` and `/account/deactivate` | Both require an access token, which the spec allows: the server may offer different flows depending on whether a token is given. Password reset by email is not offered. | Compliant. |
| Application services and `sender_localpart` | `setup/config/config_appservice.go` adds the `sender_localpart` user to the service's exclusive user namespaces, so events concerning it are delivered. | Implemented. |

### v1.12

The [v1.12 changes](https://spec.matrix.org/v1.12/changelog/) add account locking, media `Content-Type` and `Content-Disposition` rules, `via` on joins and knocks, guest access to authenticated media, and marking rooms as unread.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Account locking (MSC3939) | `internal/httputil/httpapi.go` refuses locked accounts with 401 `M_USER_LOCKED` and `soft_logout: true` on every authenticated route except `/logout` and `/logout/all`; `clientapi/routing/login.go` and `refresh.go` refuse new tokens. `clientapi/routing/admin_moderation.go` serves `GET` and `PUT /v1/admin/lock/{userId}` (MSC4323). `clientapi/account_lock_test.go` and `internal/httputil/locked_test.go` cover them. | Implemented; with MSC3861 the authentication service locks accounts. |
| Media `Content-Type` and `Content-Disposition` (MSC2701, MSC2702) | `mediaapi/routing/upload.go` stores uploads without a type as `application/octet-stream`; `download.go` serves that type for untyped media and an `inline` disposition with a file name for thumbnails. `content_disposition_test.go` covers both. | Implemented. |
| `via` on joins and knocks (MSC4156) | `clientapi/routing/joinroom.go` reads `via`, falling back to the deprecated `server_name`. | Implemented. |
| Guest access to authenticated media (MSC4189) | `mediaapi/routing/routing.go` serves the `/v1/media` download and thumbnail routes through `MakeHTTPAPI`, which admits guests. | Implemented. |
| 403 on profile lookups (MSC4170) | The response is allowed, not required. Zendrite does not restrict lookups of its own profiles, and `clientapi/routing/profile.go` passes on a remote server's 403. | Compliant. |
| Marking rooms as unread (MSC2867) | `m.marked_unread` is room account data, which needs no server support. | Client-only. |

### v1.13

The [v1.13 changes](https://spec.matrix.org/v1.13/changelog/) add room reports, account suspension, ephemeral data for application services, requestToken error codes, ACLs for EDUs, and the order of claimed one-time keys.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Reporting rooms (MSC4151) | `clientapi/routing/report_event.go` serves `POST /v3/rooms/{roomId}/report` and stores reports in `roomserver_reported_rooms`; `clientapi/room_report_test.go` covers reports, errors and the admin listing. | Implemented. |
| Account suspension (MSC3823) | `internal/httputil/httpapi.go` refuses suspended accounts on routes registered with `WithDenySuspended`, and `generateSendEvent` and `checkRedactionAllowed` limit their events; `clientapi/account_suspension_test.go` covers allowed and refused actions. | Implemented; optional. |
| Ephemeral data for application services (MSC2409) | `appservice/consumers/ephemeral.go` delivers typing, receipts and presence to application services with `receive_ephemeral`; `appservice/ephemeral_test.go` covers interest, private receipts, typing timeouts and the unstable field. | Implemented. |
| requestToken error codes (MSC4178) | `clientapi/routing/threepid.go` returns `M_INVALID_PARAM` for invalid addresses and `M_THREEPID_MEDIUM_NOT_SUPPORTED` without a validation server; the `msisdn/requestToken` routes return `M_THREEPID_MEDIUM_NOT_SUPPORTED`. | Implemented. |
| ACLs for EDUs (MSC4163) | `internal/transactionrequest.go` drops typing and receipt EDUs from servers that a room's ACL denies. | Implemented. |
| One-time key order (MSC4225) | `keyserver_one_time_keys` claims select the oldest `ts_added_secs` first. | Implemented; keys uploaded within the same second have no defined order. |
| Optional report `reason` (MSC2414), CORS methods (MSC4138), reply fallbacks (MSC2781) | `ReportEvent` accepts a missing `reason`; CORS responses list `GET, POST, PUT, DELETE, OPTIONS`; reply fallbacks are a client concern. | Compliant. |

### v1.14

The [v1.14 changes](https://spec.matrix.org/v1.14/changelog/) add user reports, remove the `server_name` parameter from `/join` and `/knock`, and make room version 11 the default.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Reporting users (MSC4260) | `clientapi/routing/report_event.go` serves `POST /v3/users/{userId}/report` and stores reports in `roomserver_reported_users`; `clientapi/user_report_test.go` covers local and remote users, errors and the admin listing. | Implemented; reports of remote users are accepted without a check. |
| `server_name` removed from `/join` and `/knock` (MSC4213) | `clientapi/routing/joinroom.go` and `knock.go` read `via` and only fall back to `server_name` when `via` is absent. | Compliant; the fallback keeps older clients working. |
| Default room version 11 (MSC4239) | `room_server.default_room_version` defaults to room version 12, which `/capabilities` reports as `default`. | Compliant; the default is the server's choice, and the spec recommends a stable version, which 12 is. |

### v1.15

The [v1.15 changes](https://spec.matrix.org/v1.15/changelog/) add room summaries, rich text topics, the OAuth 2.0 authentication API, device keys in Olm-encrypted events, and clarify what "public" rooms are.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Room summaries (MSC3266) | `clientapi/routing/room_summary.go` serves `/v1/room_summary/{roomIdOrAlias}`; `room_summary_test.go` tests access, response shape, and caching. `roomserver/internal/query/query_room_hierarchy.go` adds `room_version` and `encryption` to client and federation `/hierarchy` rooms; `clientapi/spec_v115_test.go` covers them. | Implemented; the fields come from gomatrixserverlib's `fclient.PublicRoom`, which uses the stable keys. |
| Rich text topics (MSC3765) | `internal/eventutil/eventcontent.go` builds `createRoom` topics with a `text/plain` `m.topic` block and extracts the plain text topic, which the room directory, `/hierarchy`, room summaries and the search index use. `eventcontent_test.go` and `clientapi/spec_v115_test.go` cover them. | Implemented. |
| OAuth 2.0 API (MSC3861, MSC2965) | With [MSC3861](/mscs/#msc3861-oidc-delegated-authentication), `clientapi/routing/routing.go` serves `/v1/auth_metadata` and delegates authentication; otherwise the route is absent and returns 404 `M_UNRECOGNIZED`, which the spec prescribes for servers without the API. | Compliant; the OAuth API is optional. |
| Device keys in Olm events (MSC4147) | Olm payloads are opaque to the server. | Client-only. |
| "Public" rooms clarified for profiles and the user directory | Profile lookups are not restricted; the user directory searches all local users and the users who share a room with the requester, which the spec allows. | Compliant; the matching clarifications for presence, calls and policy lists add no server requirement. |

### v1.16

The [v1.16 changes](https://spec.matrix.org/v1.16/changelog/) add custom profile fields, `state_after` in `/sync`, room version 12, the time zone profile field, the create event in invite and knock state, and the `format` parameter on state lookups.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Custom profile fields (MSC4133) and `m.tz` (MSC4175) | `clientapi/routing/profile.go` serves every profile field at the v3 and unstable paths, with the `m.profile_fields` capability. | Implemented; the deprecated `m.set_displayname` and `m.set_avatar_url` capabilities are still reported. |
| `state_after` (MSC4222) | `syncapi/sync/request.go` reads `use_state_after`, and joined and left rooms then carry `state_after`. | Implemented. |
| `format` on `GET /rooms/{roomId}/state/{eventType}/{stateKey}` | `clientapi/routing/routing.go` returns the whole event when `format=event`, and the content otherwise. | Implemented. |
| Room version 12 (MSC4289, MSC4291, MSC4297, MSC4304, MSC4307) | gomatrixserverlib implements the version, including create event room IDs, privileged creators, the new state resolution and auth events from the same room. `clientapi/routing/createroom.go` merges `trusted_private_chat` invitees into `additional_creators`, `upgrade_room.go` accepts `additional_creators`, and `internal/eventutil/eventcontent.go` raises `m.room.tombstone` to 150 in rooms with privileged creators. | Implemented; room version 12 is the default. |
| Create event in stripped state (MSC4311) | Invites and knocks carry full PDUs including `m.room.create`, validated from room version 12. | Implemented. |
| No intentional mentions from replies (MSC4142) | A client concern. | Client-only. |

### v1.17

The [v1.17 changes](https://spec.matrix.org/v1.17/changelog/) remove legacy mentions, add device masquerading and device management for application services, and add the `m.oauth` UIA type.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Legacy mentions removed (MSC4210) | `internal/pushrules` no longer installs `.m.rule.contains_display_name`, `.m.rule.contains_user_name` and `.m.rule.roomnotif`, and drops them from stored rule sets; `mentions_test.go` covers the upgrade. | Implemented. |
| Device masquerading (MSC4326) | `clientapi/auth/auth.go` reads `device_id` and `org.matrix.msc3202.device_id`; `userapi/internal/user_api.go` resolves the device and answers `M_UNKNOWN_DEVICE`. | Implemented. |
| Device management for application services (MSC4190) | `clientapi/routing/device.go` creates devices with 201 and deletes them without UIA; with MSC3861, `M_APPSERVICE_LOGIN_UNSUPPORTED` refuses logins. `clientapi/spec_v117_test.go` and `msc3861_test.go` cover them. | Implemented. |
| `m.oauth` UIA (MSC4312) | `clientapi/routing/key_crosssigning.go` offers `m.oauth` when replacing cross-signing keys with MSC3861. | Implemented; only relevant with the OAuth API. |
| `device_one_time_keys_count` | `/sync` always includes it (empty when no keys are left), and the sliding sync E2EE extension reports `signed_curve25519` as zero then. | Compliant. |
| `room_id` required when peeking with `/events` | `GET /v3/events` is a deprecated legacy endpoint that Zendrite answers with an empty chunk, as before v1.17. | Not a change in behavior; see [related findings](#related-route-audit-findings). |

### v1.18

The [v1.18 changes](https://spec.matrix.org/v1.18/changelog/) add administrator lock and suspend endpoints, OAuth 2.0 account management and device authorization, auto-forget on leave, redactions through `/send`, invite blocking, and policy servers.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Admin lock and suspend (MSC4323) | `clientapi/routing/admin_moderation.go` serves `GET` and `PUT /v1/admin/lock/{userId}` and `/suspend/{userId}`, with the `m.account_moderation` capability. | Implemented. |
| OAuth 2.0 account management (MSC4191), OAuth-aware clients (MSC3824), device authorization grant (MSC4341) | With MSC3861, `/v1/auth_metadata` serves the provider's `account_management_uri`, `account_management_actions_supported` and grant types, and `GET /login` marks `m.login.sso` as `oauth_aware_preferred`. | Implemented; the grant itself is the provider's. |
| Forget on leave (MSC4267) | `room_server.auto_forget_on_leave` sets the `m.forget_forced_upon_leave` capability. | Implemented; optional. |
| Redactions through `/send` (MSC4169) | `clientapi/routing/sendevent.go` copies `content.redacts` to the top level. | Implemented. |
| Invite blocking (MSC4380) | `m.invite_permission_config` refuses invites with 403 `M_INVITE_BLOCKED` and hides them from `/sync`; `clientapi/spec_v118_test.go` covers every endpoint. | Implemented. |
| Policy servers (MSC4284) | `roomserver/internal/input/policy_server.go` checks every new event with the room's policy server and fails closed; `roomserver/policy_server_test.go` covers it. | Implemented. |
| `M_USER_LIMIT_EXCEEDED` (MSC4335) and `submitToken` error codes (MSC4183) | Zendrite has no per-user quotas and never returns a `submit_url`. | Not applicable. |
| Report `score` removed (MSC4277); reports may answer 200 for unknown subjects | `ReportEvent` no longer requires `score` and stores it when older clients send it; answering 404 for unknown subjects stays allowed. | Compliant. |
| `/v1/send_join` and `/v1/send_leave` removed (MSC4376) | Both are still served for older servers, next to the v2 endpoints. | Compliant. |
| Recent emoji (MSC4356), animated images (MSC4230), `ol` start (MSC4313), cross-signed devices (MSC4153) | Account data and client rendering. | Client-only. |

### v1.19

The [v1.19 changes](https://spec.matrix.org/v1.19/changelog/) add mutual rooms, `unsigned.replaces_state`, server-defined directory order, image packs, key backup account data, encrypted history sharing, and case-insensitive server ACLs.

| Requirement | Implementation evidence | Assessment |
| --- | --- | --- |
| Mutual rooms (MSC2666) | `clientapi/routing/mutual_rooms.go` serves `GET /v1/mutual_rooms` with signed pagination tokens and 400 `M_INVALID_PARAM` for the caller's own or an invalid user ID; `clientapi/mutual_rooms_test.go` covers it. | Implemented; also advertised as `uk.half-shot.msc2666.query_mutual_rooms.stable`. |
| `unsigned.replaces_state` | `syncapi/consumers/roomserver.go` records the event a state event replaces, which `syncapi/synctypes/clientevent.go` serves. | Implemented. |
| Server-defined directory order (MSC4423) | The directory lists rooms by room ID, which the spec now allows. | Compliant. |
| Case-insensitive server ACLs (MSC4436) | `roomserver/acls/acls.go` compiles `allow` and `deny` globs case-insensitively. | Implemented. |
| Policy server signatures from an event's own origin | `addPolicyServerSignature` adds the policy server's key to the server's existing signatures instead of replacing them. | Compliant. |
| Image packs (MSC2545), key backup account data (MSC4287), encrypted history sharing (MSC4268) | State events, account data, media and to-device messages that the server stores without interpreting. | Client-only. |

### Related route-audit findings

- **3PID management:** `clientapi/routing/routing.go` provides listing, legacy association, deletion, and email token requests, but lacks the distinct `/account/3pid/add`, `/bind`, `/unbind`, and MSISDN token routes.
  Password reset by email (`/account/password/email/requestToken` and `/account/password` without an access token) works when `client_api.email_validation_server` is set; MSISDN password reset returns `M_THREEPID_MEDIUM_NOT_SUPPORTED`.
  MSC3861 intentionally disables local account mutation and delegates account management; that configuration is not evidence for native 3PID support.
- **Mutual rooms:** `clientapi/routing/mutual_rooms.go` serves the stable `GET /v1/mutual_rooms` from v1.19.
- **Legacy `/events` and `/initialSync`:** both are deprecated and answered with empty responses, kept for Sytest.
  Clients use `/sync` instead.
- **Notifications ([#312](https://codefloe.com/pat-s/zendrite/issues/312)):** `GetNotifications` now validates query parameters and calls `QueryNotifications`; `notification_test.go` covers response behavior.
  This is not a stub on the audited branch and is not listed as an unresolved blocker here.
- **Room summaries ([#314](https://codefloe.com/pat-s/zendrite/issues/314)):** Stable and compatibility routes are present, backed by `GetRoomSummary` and focused tests.
  `room_version` and `encryption` in `/hierarchy` complete MSC3266 (see v1.15).

## Experimental feature discovery

`clientapi/routing/routing.go` builds the feature map at startup.
Unknown or inactive MSC names in configuration must not turn into advertised support or override configuration-derived flags.
The explicit configurable list mirrors the implemented cases in `setup/mscs/mscs.go`.

| Flag | Advertisement condition | Implementation evidence |
| --- | --- | --- |
| `org.matrix.e2e_cross_signing` | Always, existing compatibility flag. | Signing-key upload and signature routes and handlers in `clientapi/routing`. |
| `org.matrix.msc2285.stable` | Always, existing compatibility flag. | Private receipts and optional read markers, including filtering in the sync receipt stream. |
| `org.matrix.msc3916.stable` | Always, existing compatibility flag. | Authenticated download, thumbnail, and configuration handlers in `mediaapi/routing/routing.go`. |
| `org.matrix.simplified_msc3575` | Always, existing compatibility flag. | Native sliding-sync handler and compatibility route in `syncapi/routing/routing.go`. |
| `org.matrix.msc4143` | True only with nonempty `global.rtc_foci`; false otherwise. | RTC transport endpoint and well-known focus discovery share that configuration. |
| `org.matrix.msc4354` | Always. | Sticky events: `org.matrix.msc4354.sticky_duration_ms` on `/send` and the `msc4354_sticky` section of `/sync`. |
| `com.beeper.msc4169` | Always. | Redactions through `PUT /send/m.room.redaction` in every room version. |
| `uk.half-shot.msc2666.query_mutual_rooms.stable` | Always. | The stable `GET /v1/mutual_rooms`, for clients that check the flag rather than v1.19. |
| `org.matrix.msc4222` | Always. | The `use_state_after` and `org.matrix.msc4222.use_state_after` `/sync` parameters in `syncapi/sync/request.go`, for clients that check the flag before opting in. |
| `org.matrix.msc4140` | True only with a positive `client_api.delayed_events.max_event_delay_duration` and `max_delayed_events_per_user`; false otherwise. | Delayed event scheduling, management and scheduler in `clientapi/routing/delayed_events.go`, with the `org.matrix.msc4140.delayed_events` capability. |
| `org.matrix.msc4140.stable` | Same as `org.matrix.msc4140`. | The stable `/v3/rooms/{roomId}/delayed_event` and `/v1/delayed_events/{delayId}` endpoints, with the `m.delayed_events` capability. |
| `org.matrix.msc4380`, `org.matrix.msc4380.stable` | Always. | Invite blocking through the `org.matrix.msc4380.invite_permission_config` and `m.invite_permission_config` account data: `InvitesBlocked` in `userapi/api/invite_permission.go`, checked by the roomserver invite paths and the sync invite streams. |
| `uk.tcpip.msc4133`, `uk.tcpip.msc4133.stable` | Always. | Profile field endpoints at the unstable and v3 paths in `clientapi/routing/profile.go`, with the `m.profile_fields` capability. |
| `org.matrix.msc2444`, `org.matrix.msc2753`, `org.matrix.msc2836`, `org.matrix.msc3814` | Only when the respective implemented MSC is configured. | Federation peeking, client peeking, experimental relations, and dehydrated-device modules respectively. |
| `org.matrix.msc3861` | Only when MSC3861 is enabled. | Delegated authentication routes and token introspection. |
| `org.matrix.msc2965`, `org.matrix.msc2967` | Implied by MSC3861, never enabled by listing those names alone. | Authentication metadata and OAuth scope integration. |

These retained flags describe existing feature implementations, not an exhaustive certification of every proposal revision.
MSC4115 membership annotation is already applied in sync; listing `msc4115` does not activate a separate implementation and no longer synthesizes an `org.matrix.msc4115` flag.
Likewise, listing `msc4143` alone cannot advertise RTC availability without configured foci.

## Verification and promotion procedure

`clientapi/routing/versions_test.go` exercises the real client router with a user API that cannot service any calls, proving discovery needs neither authentication nor storage.
It checks the exact version list and feature map with default settings, supported opt-ins, duplicate and unknown entries, OIDC on/off, RTC on/off, unauthenticated requests, invalid bearer credentials, and CORS preflight.
The existing `TestMSC3861_VersionsAdvertisesMSC2965` additionally covers discovery with a configured user API stack.

```sh
# Run discovery regression tests.
go test -tags goolm ./clientapi/routing -run 'TestVersions|TestMSC3861_VersionsAdvertisesMSC2965' -count=1
```

Before raising the ceiling, fix any remaining blockers and add behavioral tests for newly required endpoints.
Then review every server requirement and applicable clarification in the candidate release, run the relevant conformance suites in supported authentication configurations, and record that evidence here alongside the version-list change.
Do not equate a passing discovery test with protocol conformance.
