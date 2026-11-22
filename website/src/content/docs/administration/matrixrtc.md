---
title: MatrixRTC (voice and video calls)
description: Advertise a LiveKit focus server to clients so that Element Call works with Zendrite.
---

Element Call uses MatrixRTC ([MSC4143](https://github.com/matrix-org/matrix-spec-proposals/pull/4143)) for voice and video.
Call media never passes through Zendrite.
Clients connect to a [LiveKit](https://livekit.io/) SFU and get a token for it from [lk-jwt-service](https://github.com/element-hq/lk-jwt-service).
If Zendrite has the SFU's API key, Zendrite can issue the token instead (see [issuing tokens in Zendrite](#issuing-tokens-in-zendrite)).
Otherwise Zendrite only tells clients where to find that service.

None of this has an effect until you run a working LiveKit SFU and lk-jwt-service.
[Deploying Element Call](#deploying-element-call) walks through the whole setup.
The sections before it cover the Zendrite side.

## Configuration

Add each focus server to the `rtc_foci` list in the `global` section of the configuration file:

```yaml
global:
  # ...
  rtc_foci:
    - type: livekit
      livekit_service_url: "https://matrix.example.com/livekit/jwt"
```

`livekit_service_url` is the URL of **lk-jwt-service**, not of the LiveKit SFU.
Clients learn the SFU address from the token that the service returns.

Zendrite validates the list at startup.
It refuses to start if a `livekit` focus has neither a service URL nor an SFU URL.

### Issuing tokens in Zendrite

Zendrite can issue the LiveKit tokens itself instead of lk-jwt-service, as [MSC4195](https://github.com/matrix-org/matrix-spec-proposals/pull/4195) describes.
Give the focus the SFU's WebSocket URL and the API key and secret that the SFU uses:

```yaml
global:
  rtc_foci:
    - type: livekit
      url: "wss://livekit.example.com"
      livekit_api_key: "APIxxxxxxxx"
      livekit_api_secret: "the secret"
      # Optional: the SFU's HTTP API, if it is not the url with an http(s) scheme.
      # livekit_api_url: "http://livekit.internal:7880"
```

Zendrite never shows the key and secret to clients, but it does show the `url`.
You can keep `livekit_service_url` next to `url` for clients that still ask lk-jwt-service.
With a key, Zendrite:

- Serves `POST /_matrix/client/v1/rtc/livekit/get_token` and `/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token` to users, guests, and call guests, with rate limiting.
  A user joined to the room gets a token for the SFU after Zendrite creates the LiveKit room for the call through the SFU's API.
  Zendrite refuses a `url` that is not one of the configured SFUs with `M_INVALID_PARAM`.
- Serves the federation side, `POST /_matrix/federation/v1/rtc/livekit/get_token` and its unstable path, to users of other servers joined to the room.
  It refuses servers that the room's server ACL denies and accepts at most 5 requests a second per server.
- Advertises `org.matrix.msc4195.stable` in `unstable_features`.

With or without a key, when a request names another `server_name` that is in the room, Zendrite asks that server over federation for a token for its SFU.
Zendrite passes on that server's `M_FORBIDDEN` and `M_INVALID_PARAM` errors.

Tokens last an hour and may publish and subscribe.
They can't create, delete, or hide in rooms.
Zendrite doesn't create a room on the SFU again within a minute of creating it.
LiveKit room names are hashes of the Matrix room and slot, and participant identities are hashes of the user and member ID, so the SFU doesn't learn them.

### Removing members who leave

When a user leaves or is banned from a Matrix room, Zendrite removes them from the LiveKit rooms of that room's calls, as MSC4195 recommends.
This applies to every local and remote user that Zendrite issued a token to for those calls.
The removal also revokes the tokens issued so far, but only LiveKit Cloud and LiveKit versions with token revocation honor that.

A self-hosted SFU without revocation lets the removed participant rejoin with their token until it expires.
To catch this, Zendrite checks again whenever the SFU reports a participant joining, and removes them if they are no longer joined to the Matrix room.
This needs the SFU's webhooks pointed at Zendrite (see [SFU webhooks](#sfu-webhooks)).

Zendrite keeps the participants it issued tokens to in memory for the hour that the tokens last.
After a restart or an HA failover, it can't remove the holders of tokens it issued before.
Those tokens still expire within the hour.
Zendrite doesn't remove participants who got their token from lk-jwt-service or another server.

### Delegated delayed leave events

Element Call can hand its delayed leave event to the homeserver with `POST /_matrix/client/v1/rtc/livekit/delegate_delayed_leave` (and `/_matrix/client/unstable/io.element.msc4195/rtc/livekit/delegate_delayed_leave`).
The homeserver then sends the leave as soon as the client drops off the SFU, instead of when the delay runs out.
Zendrite serves the endpoint when it has an API key and [delayed events](#delayed-events) are enabled.
The endpoint is rate limited and open to guests and call guests.
It works only for Zendrite's own SFUs, as the MSC says.

The request fails with `M_INVALID_PARAM` unless these conditions hold:

- `delay_id` is a delayed event that the user has scheduled in `room_id`.
- The delay is at least 10 seconds.
- `url` is one of Zendrite's SFUs.

Element Call delegates a delay of an hour.
A new delegation for the same member, or for the same delayed event, replaces the earlier one.
Zendrite then:

- Restarts the delayed event once half its delay has passed, and at the latest at three quarters of it, while the participant is connected or still expected.
- Asks the SFU every 15 seconds, or sooner for short delays, whether the participant is connected, and follows the SFU's webhooks between checks.
- Sends the leave event once the participant has disconnected, or has not connected within two minutes.
  Webhooks may arrive late, so Zendrite confirms a disconnect that a webhook reports with the SFU first.
  A connection that replaced an older one of the same participant doesn't count as a disconnect.
- Stops when the delayed event is sent, canceled, or no longer scheduled.
- Leaves the delayed event to its own delay when the SFU has not answered for two minutes.

Zendrite keeps delegations in memory.
After a restart or an HA failover, Zendrite sends each delayed event when its own delay runs out, as without delegation, and Element Call keeps restarting it too.

### SFU webhooks

Zendrite accepts LiveKit webhooks at `POST /_zendrite/livekit/webhook`.
Each webhook must have an expiry and be signed with the API key and secret of one of Zendrite's SFUs.
Zendrite refuses others with 401.
With webhooks, removals and delegated leaves react at once instead of at the next check.
Configure them in the SFU's `livekit.yaml` with the same key that Zendrite uses:

```yaml
webhook:
  api_key: "APIxxxxxxxx"
  urls:
    - "https://matrix.example.com/_zendrite/livekit/webhook"
```

Only the SFU calls this URL, so it may also be an address on the internal network.
Without webhooks, delegated leave events still work through the checks every 15 seconds.
A participant removed from a self-hosted SFU can then rejoin with their token unnoticed.

### lk-jwt-service as an application service

lk-jwt-service can also serve the `rtc/livekit` endpoints behind Zendrite as an application service.
This uses [MSC4512](/mscs/#msc4512-application-services-serving-parts-of-the-apis) and [MSC4502](/mscs/#msc4502-room-member-queries), which the service uses to check room membership.
Enable both:

```yaml
mscs:
  mscs:
    - msc4502
    - msc4512
```

Then register lk-jwt-service with the registration file from its README.
That file claims `rtc/livekit` with `io.element.msc4512.proxy_prefix` and points `io.element.msc4512.proxy_url` at the service.
Zendrite forwards the client and federation `rtc/livekit` requests to the service, and the service reaches other servers through Zendrite's `fed_proxy` endpoint.
In this setup, leave the LiveKit API key out of `rtc_foci`.
The application service takes over `get_token` and `delegate_delayed_leave` either way.

## What clients see

With at least one focus configured, Zendrite exposes the list in three places:

- `/.well-known/matrix/client` gains an `org.matrix.msc4143.rtc_foci` key.
- `GET /_matrix/client/v1/rtc/transports` returns the list as `transports` to authenticated clients, guests, and call guests.
- `GET /_matrix/client/unstable/org.matrix.msc4143/rtc/transports` returns it as `rtc_transports` to the same users, for clients that still use the unstable endpoint.

All three use the flat format that clients expect:

```json
"org.matrix.msc4143.rtc_foci": [
  {
    "type": "livekit",
    "livekit_service_url": "https://matrix.example.com/livekit/jwt"
  }
]
```

A focus with a `url` (MSC4195) also includes it as `url`.

`GET /_matrix/client/versions` advertises `org.matrix.msc4143` in `unstable_features` only while at least one focus is configured.
It always advertises `org.matrix.msc4143.stable`, as Synapse (and so matrix.org) does, because Zendrite always serves the stable endpoint.
Without `rtc_foci`, the stable endpoint returns an empty `transports` list, and clients find no focus.

## Call membership

Element Call announces who is in a call with `m.rtc.member` events that it sends as sticky events ([MSC4354](https://github.com/matrix-org/matrix-spec-proposals/pull/4354)).
Zendrite supports them without configuration.
It advertises `org.matrix.msc4354` in `unstable_features`, which Element Call checks before using them.
Element Call schedules its leave membership as a sticky event with [delayed events](#delayed-events), and Zendrite keeps it sticky when it sends it.
See [sticky events](/mscs/#sticky-events) for how Zendrite delivers them.

## Delayed events

Element Call keeps a "leave" call membership event scheduled on the server with [MSC4140](https://github.com/matrix-org/matrix-spec-proposals/pull/4140) delayed events.
It restarts the timer every few seconds while the call runs.
If the client crashes or loses its connection, the server sends the leave event when the timer runs out, and the user drops out of the call for everyone else.
Without delayed events, such a user stays in the call until their membership expires.

Delayed events are disabled by default, as in Synapse.
Enable them by setting a maximum delay in the `client_api` section:

```yaml
client_api:
  delayed_events:
    max_event_delay_duration: 24h
    max_delayed_events_per_user: 100
    management_rate_limiting:
      enabled: true
      threshold: 20
      cooloff_ms: 20000
```

- `max_event_delay_duration` is the longest delay a client may request.
  `0s` disables delayed events.
- `max_delayed_events_per_user` caps how many events one user may have scheduled at once.
  `0` also disables delayed events.
- `management_rate_limiting` limits the requests that restart, cancel, send, or look up delayed events.
  It counts per device, or per address for requests without an access token.
  A request takes one of `threshold` slots for `cooloff_ms` milliseconds.
  The defaults above allow a burst of 20 requests and then one per second, which Element Call's documentation suggests for Synapse.

The general `client_api.rate_limiting` limits scheduling an event instead.

See [MSC support](/mscs/#delayed-events) for the endpoints and their behaviour.

## Call guests

To let people without a Matrix account join calls by link through the standalone Element Call app, enable [call guests](/administration/registration/#call-guests).
Call guests are restricted accounts that expire, so normal registration can stay closed.
They are local users, so lk-jwt-service gives them full access when your homeserver is in `LIVEKIT_FULL_ACCESS_HOMESERVERS`.

## Deploying Element Call

A complete Element Call setup has these parts:

| Part | Role |
| --- | --- |
| Zendrite | Advertises the focus, stores call memberships, and sends delayed leave events |
| [lk-jwt-service](https://github.com/element-hq/lk-jwt-service) | Checks a user's Matrix OpenID token and returns LiveKit access tokens |
| [LiveKit](https://livekit.io/) | The SFU that forwards the call media |
| Valkey or Redis | Shared state, required once LiveKit or lk-jwt-service runs more than one instance |
| The Element Call app | Optional: the standalone web app for calls by link, as on call.element.io |

Element Web and Element X include Element Call and find the focus through `.well-known`, so they need no extra app.
You need the standalone app only for a dedicated call page and for [call guests](/administration/registration/#call-guests).

### Homeserver requirements

Element Call relies on these features, which Zendrite supports:

- [MSC4143](https://github.com/matrix-org/matrix-spec-proposals/pull/4143) foci, configured with [`rtc_foci`](#configuration).
- [MSC4140](https://github.com/matrix-org/matrix-spec-proposals/pull/4140) [delayed events](#delayed-events), which are off by default.
  Set `max_event_delay_duration`, for example to `24h`.
- [MSC4354](https://github.com/matrix-org/matrix-spec-proposals/pull/4354) sticky events, without configuration.
- [MSC3266](https://github.com/matrix-org/matrix-spec-proposals/pull/3266) room summaries, which the standalone app uses to decide whether it can join a room from a link.
- `POST /_matrix/client/v3/user/{userId}/openid/request_token`, which lk-jwt-service validates.

Zendrite creates rooms with room version 12 by default.
In that version, a room's creators outrank every power level and must not be listed in its power levels.
Element Call still lists the creator when it creates a call room.
Zendrite drops creators from such a `power_level_content_override`, so Element Call can create rooms.
Older Zendrite builds refuse the request with `new power levels event must not contain creator`.

### With the Helm chart

The [Helm chart](/installation/helm/helm/#element-call) deploys LiveKit and lk-jwt-service next to Zendrite with its `matrixRTC` values.
It sets `rtc_foci` and delayed events, and routes a dedicated host to them.
It needs a Valkey or Redis that always accepts writes.
For a highly available Valkey, use the [valkey-io chart](https://github.com/valkey-io/valkey-helm/tree/main/valkey) with Sentinel and its HAProxy front end, and point the chart at `<release>-haproxy:6379`.

### Without the Helm chart

Deploy lk-jwt-service and LiveKit as their own documentation describes, with these settings to connect them to Zendrite:

- lk-jwt-service:
  - `LIVEKIT_URL` is the public WebSocket URL of LiveKit, for example `wss://rtc.example.com`.
  - `LIVEKIT_KEY` and `LIVEKIT_SECRET` match one of LiveKit's API keys.
  - `LIVEKIT_FULL_ACCESS_HOMESERVERS` lists the server names whose users may start calls.
    Users of other servers can only join calls already running.
  - `LIVEKIT_CS_API_URL_OVERRIDES` (`example.com=http://zendrite:8008`) lets the service reach Zendrite's client API without going through the public address.
  - With more than one replica, `LIVEKIT_REDIS_URL` and `LIVEKIT_REDIS_PASSWORD` point the service at the shared Valkey.
- LiveKit:
  - `redis` points at the shared Valkey, so that several LiveKit nodes can share rooms.
  - `rtc.tcp_port` and `rtc.udp_port` are the media ports, which clients reach directly on each node.
    Open them in the firewall.
  - `rtc.use_external_ip: true` makes each node find its public address through STUN.
  - `room.auto_create: false` leaves room creation to lk-jwt-service.
  - A `webhook` to lk-jwt-service's `/sfu_webhook` lets the service send the delayed leave event of a client that crashed.
- `rtc_foci` in Zendrite points at lk-jwt-service's public URL.
  When LiveKit is served at `/` of its own host, the same host can route `/sfu/get`, `/get_token`, and `/delegate_delayed_leave` to lk-jwt-service.

### The Element Call app

The standalone app is a static site, published as `ghcr.io/element-hq/element-call`.
The image serves it with nginx on port 8080 as an unprivileged user and reads its configuration from `/app/config.json`.
The app discovers the focus through `.well-known`, so the configuration needs only the homeserver:

```json
{
  "default_server_config": {
    "m.homeserver": {
      "base_url": "https://matrix.example.com",
      "server_name": "example.com"
    }
  }
}
```

Element Call also accepts a LiveKit setting in `config.json`, but documents it as for development only.
With a read-only root filesystem, the container needs writable `/tmp` and `/var/cache/nginx`.

Users of your homeserver log in to the app, start calls, and share their links.
To let people without an account join by link, enable [call guests](/administration/registration/#call-guests).
Without call guests, the app tries to register an account for them, which fails while registration is closed.

### Checking the setup

```bash
# the focus Zendrite advertises
curl -s https://matrix.example.com/.well-known/matrix/client
# org.matrix.msc4140, org.matrix.msc4143 and org.matrix.msc4354 in unstable_features
curl -s https://matrix.example.com/_matrix/client/versions
# lk-jwt-service answers; without a room it returns 400 M_BAD_JSON
curl -s -X POST -H 'Content-Type: application/json' -d '{}' https://rtc.example.com/sfu/get
```

If calls connect but have no audio or video, the media ports are usually blocked, most often UDP.

### Sizing

LiveKit's [benchmark](https://docs.livekit.io/home/self-hosting/benchmark/) ran 150 participants with video on 16 vCPUs at 85% CPU.
That works out to about 0.1 vCPU per participant with video.
Element Call sends video at up to 720p and 1.7 Mbit/s.
A participant uploads about 2.5 Mbit/s and downloads up to several Mbit/s, depending on how many others they see.

Audio-only calls need a small fraction of that, and LiveKit needs little memory.
A call runs on a single LiveKit node, so one node's CPU limits the size of a call, while separate calls spread over several nodes.

## Deprecated nested configuration

Earlier Zendrite versions documented a nested form:

```yaml
global:
  rtc_foci:
    - type: livekit
      livekit:
        service_url: "https://matrix.example.com/livekit/jwt"
```

Zendrite still accepts that form and produces the same flat output, so existing configurations keep working.
Use `livekit_service_url` in new deployments.
