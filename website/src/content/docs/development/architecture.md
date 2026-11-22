---
title: Architecture
description: How Zendrite and gomatrixserverlib divide the work.
---

Zendrite uses [gomatrixserverlib](https://codefloe.com/pat-s/gomatrixserverlib), a Go library that implements the core Matrix protocol.
Before you contribute or implement a new [MSC](https://spec.matrix.org/proposals/), learn where the boundary between the two runs.

## gomatrixserverlib (protocol layer)

gomatrixserverlib is a standalone library with no knowledge of Zendrite.
It provides the building blocks of the Matrix protocol:

- **Event types and formats:** PDU interface, event versions (V1 to V12), and event builders.
- **Room versions:** version metadata, stability flags, and format differences.
- **State resolution:** algorithms that resolve conflicting room state (V1, V2, and V2.1).
- **Event authentication:** rules that validate events against auth state.
- **Federation types:** request and response structs (`PublicRoom`, `RoomHierarchyRoom`, `RespSend`, and others).
- **Error codes:** Matrix spec error types (`M_FORBIDDEN`, `M_NOT_FOUND`, `M_TOO_LARGE`, and others).
- **Cryptographic operations:** event signing and signature verification.
- **Redaction logic:** spec-compliant stripping of event content.
- **Identity types:** user IDs, room IDs, server names, and sender IDs.

## Zendrite (application layer)

Zendrite is the homeserver application that uses gomatrixserverlib.
It handles everything above the protocol layer:

- **HTTP endpoints:** routing for the Client-Server API, Federation API, and Admin API.
- **Database storage:** event persistence, state snapshots, and device data.
- **Configuration:** the default room version and the enabled MSCs.
- **Business logic:** access control, sync, presence, and push notifications.
- **MSC feature toggles:** opt-in MSCs through config (`setup/mscs/`).

## Where MSC changes go

| Change type                                                        | Where                                           |
| ------------------------------------------------------------------ | ----------------------------------------------- |
| New error codes (for example `M_TOO_LARGE`)                        | gomatrixserverlib (`spec/`)                     |
| New fields on protocol types (for example `PublicRoom.Encryption`) | gomatrixserverlib (`fclient/`)                  |
| New room version definitions or state resolution changes           | gomatrixserverlib (`eventversion.go`)           |
| New event types or content formats                                 | gomatrixserverlib (`spec/`)                     |
| New API endpoints or handlers                                      | Zendrite (`clientapi/`, `federationapi/`, etc.) |
| New server behavior or business logic                              | Zendrite                                        |
| New configuration options                                          | Zendrite (`setup/config/`)                      |
| New database tables or queries                                     | Zendrite (`storage/`)                           |

Many MSCs need changes in **both**: protocol types in gomatrixserverlib and endpoint handlers in Zendrite.

### MSC3266 (room summary)

- **gomatrixserverlib** adds the `Encryption` and `RoomVersion` fields to the `PublicRoom` struct.
- **Zendrite** implements the `/summary` endpoint in `clientapi/routing/room_summary.go` and fills the new fields from room state.

### MSC2836 (threading)

- **gomatrixserverlib** defines the `MSC2836EventRelationshipsResponse` federation type.
- **Zendrite** implements the `/event_relationships` endpoint, the database tables for parent-child tracking, and the opt-in config toggle in `setup/mscs/msc2836/`.

## Change gomatrixserverlib

Zendrite depends on its own [gomatrixserverlib](https://codefloe.com/pat-s/gomatrixserverlib) module, `codefloe.com/pat-s/gomatrixserverlib`, at the version pinned in `go.mod`.
To change protocol types, merge the change into gomatrixserverlib first and release it.
Then bump that version in Zendrite's `go.mod`.
