---
title: MatrixRTC (voice and video calls)
description: Advertising a LiveKit focus server to clients so that Element Call works against Zendrite
---

MatrixRTC ([MSC4143](https://github.com/matrix-org/matrix-spec-proposals/pull/4143)) is the transport used by Element Call for voice and video.
The media itself never passes through Zendrite: clients connect to a [LiveKit](https://livekit.io/) SFU, and obtain a token for it from [lk-jwt-service](https://github.com/element-hq/lk-jwt-service), or from Zendrite itself if it holds the SFU's API key (see [issuing tokens in Zendrite](#issuing-tokens-in-zendrite)).
Otherwise Zendrite's only job is to tell clients where that service lives.

You therefore need a working LiveKit SFU and lk-jwt-service deployment before any of this has an effect.
[Deploying Element Call](#deploying-element-call) below walks through the whole setup; the sections before it cover the Zendrite side.

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
Clients discover the SFU itself from the token that service hands out.

Zendrite validates the list at startup and refuses to start if a `livekit` focus has neither a service URL nor an SFU URL.

### Issuing tokens in Zendrite

Instead of lk-jwt-service, Zendrite can issue the LiveKit tokens itself, as [MSC4195](https://github.com/matrix-org/matrix-spec-proposals/pull/4195) describes.
Give the focus the SFU's WebSocket URL and the API key and secret that the SFU is configured with:

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

The key and secret are never shown to clients; the `url` is.
`livekit_service_url` may stay alongside `url` for clients that still ask lk-jwt-service.
With a key, Zendrite then:

- serves `POST /_matrix/client/v1/rtc/livekit/get_token` and `/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token` to users, guests and call guests, rate limited.
  A user joined to the room gets a token for the SFU, after Zendrite creates the LiveKit room for the call through the SFU's API.
  A `url` that is not one of the configured SFUs is refused with `M_INVALID_PARAM`.
- serves the federation side, `POST /_matrix/federation/v1/rtc/livekit/get_token` and its unstable path, to users of other servers joined to the room, unless the room's server ACL denies their server, at most 5 requests a second per server.
- advertises `org.matrix.msc4195.stable` in `unstable_features`.

Whether or not it holds a key, Zendrite asks the other server, over federation, for a token for its SFU when a request names another `server_name` that is in the room, and passes its `M_FORBIDDEN` and `M_INVALID_PARAM` errors on.

Tokens last an hour, may publish and subscribe, and cannot create, delete or hide in rooms.
A room created on the SFU is not created again for a minute.
LiveKit room names and participant identities are hashes of the Matrix room and slot, and of the user and member ID, so the SFU does not learn them.
Zendrite does not yet remove participants from the SFU when they leave the Matrix room, nor handle delegated delayed leave events (`delegate_delayed_leave`), which the MSC recommends and makes optional.

## What clients see

With at least one focus configured, Zendrite exposes it in three places:

- `/.well-known/matrix/client` gains an `org.matrix.msc4143.rtc_foci` key.
- `GET /_matrix/client/v1/rtc/transports` returns the same list as `transports` to authenticated clients, guests and call guests.
- `GET /_matrix/client/unstable/org.matrix.msc4143/rtc/transports` returns it as `rtc_transports` to the same users, for clients that still use the unstable endpoint.

All three use the flat wire format that clients expect:

```json
"org.matrix.msc4143.rtc_foci": [
  {
    "type": "livekit",
    "livekit_service_url": "https://matrix.example.com/livekit/jwt"
  }
]
```

A focus with a `url` (MSC4195) carries it as `url` as well.

`GET /_matrix/client/versions` also advertises `org.matrix.msc4143` in `unstable_features`, but only while at least one focus is configured.
It always advertises `org.matrix.msc4143.stable`, as Synapse (and so matrix.org) does, since the stable endpoint is always served.
Without `rtc_foci`, the stable endpoint returns an empty `transports` list, and clients have no focus to discover.

## Call membership

Element Call announces who is in a call with `m.rtc.member` events that it sends as sticky events ([MSC4354](https://github.com/matrix-org/matrix-spec-proposals/pull/4354)).
Zendrite supports them without configuration and advertises `org.matrix.msc4354` in `unstable_features`, which Element Call checks before using them.
Element Call schedules its leave membership as a sticky event with [delayed events](#delayed-events), and Zendrite keeps it sticky when it sends it.
See [sticky events](/mscs/#sticky-events) for how they are delivered.

## Delayed events

Element Call keeps a "leave" call membership event scheduled on the server with [MSC4140](https://github.com/matrix-org/matrix-spec-proposals/pull/4140) delayed events, and restarts its timer every few seconds while the call runs.
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

`max_event_delay_duration` is the longest delay a client may request; `0s` disables delayed events.
`max_delayed_events_per_user` caps how many events one user may have scheduled at once, and `0` also disables them.
`management_rate_limiting` limits the requests that restart, cancel, send or look up delayed events, per device or, for requests without an access token, per address.
A request takes one of `threshold` slots for `cooloff_ms` milliseconds, so the defaults above allow a burst of 20 requests and then one per second, which is what Element Call's documentation suggests for Synapse.
Scheduling an event is limited by the general `client_api.rate_limiting` instead.

See [MSC support](/mscs/#delayed-events) for the endpoints and their behaviour.

## Call guests

To let people without a Matrix account join calls by link through the standalone Element Call app, enable [call guests](/administration/registration/#call-guests).
They are restricted accounts that expire, so normal registration can stay closed.
Because they are local users, lk-jwt-service gives them full access when your homeserver is in `LIVEKIT_FULL_ACCESS_HOMESERVERS`.

## Deploying Element Call

A complete Element Call setup has these parts:

| Part | Role |
| --- | --- |
| Zendrite | Advertises the focus, stores call memberships and sends delayed leave events |
| [lk-jwt-service](https://github.com/element-hq/lk-jwt-service) | Checks a user's Matrix OpenID token and hands out LiveKit access tokens |
| [LiveKit](https://livekit.io/) | The SFU that forwards the call media |
| Valkey or Redis | Shared state, required as soon as LiveKit or lk-jwt-service runs more than one instance |
| The Element Call app | Optional: the standalone web app for calls by link, as on call.element.io |

Element Web and Element X ship Element Call themselves and find the focus through `.well-known`, so they need no extra app.
The standalone app is only needed for a dedicated call page and for [call guests](/administration/registration/#call-guests).

### Homeserver requirements

Element Call relies on these features, which Zendrite supports:

- [MSC4143](https://github.com/matrix-org/matrix-spec-proposals/pull/4143) foci, configured with [`rtc_foci`](#configuration).
- [MSC4140](https://github.com/matrix-org/matrix-spec-proposals/pull/4140) [delayed events](#delayed-events), which are off by default; set `max_event_delay_duration`, for example to `24h`.
- [MSC4354](https://github.com/matrix-org/matrix-spec-proposals/pull/4354) sticky events, without configuration.
- [MSC3266](https://github.com/matrix-org/matrix-spec-proposals/pull/3266) room summaries, which the standalone app uses to decide whether it can join a room from a link.
- `POST /_matrix/client/v3/user/{userId}/openid/request_token`, which lk-jwt-service validates.

Zendrite creates rooms with room version 12 by default.
In that version a room's creators outrank every power level and must not be listed in its power levels, yet Element Call lists the creator when it creates a call room.
Zendrite drops creators from such a `power_level_content_override`, so Element Call can create rooms; older Zendrite builds refuse the request with `new power levels event must not contain creator`.

### With the Helm chart

The [Helm chart](/installation/helm/helm/#element-call) deploys LiveKit and lk-jwt-service next to Zendrite with its `matrixRTC` values, sets `rtc_foci` and delayed events, and routes a host of their own to them.
It needs a Valkey or Redis that always accepts writes; for a highly available Valkey, use the [valkey-io chart](https://github.com/valkey-io/valkey-helm/tree/main/valkey) with Sentinel and its HAProxy front end, and point the chart at `<release>-haproxy:6379`.

### Without the Helm chart

Deploy lk-jwt-service and LiveKit following their own documentation, with these settings tying them to Zendrite:

- lk-jwt-service:
  - `LIVEKIT_URL` is the public websocket URL of LiveKit, for example `wss://rtc.example.com`, and `LIVEKIT_KEY` and `LIVEKIT_SECRET` match one of LiveKit's API keys.
  - `LIVEKIT_FULL_ACCESS_HOMESERVERS` lists the server names whose users may start calls; users of other servers can only join calls already running.
  - `LIVEKIT_CS_API_URL_OVERRIDES` (`example.com=http://zendrite:8008`) lets it reach Zendrite's client API without going through the public address.
  - With more than one replica, `LIVEKIT_REDIS_URL` and `LIVEKIT_REDIS_PASSWORD` point it at the shared Valkey.
- LiveKit:
  - `redis` holds the shared Valkey, so that several LiveKit nodes can share rooms.
  - `rtc.tcp_port` and `rtc.udp_port` are the media ports, which clients reach directly on each node; open them in the firewall.
  - `rtc.use_external_ip: true` makes each node find its public address through STUN.
  - `room.auto_create: false` leaves room creation to lk-jwt-service.
  - A `webhook` to lk-jwt-service's `/sfu_webhook` lets it send the delayed leave event of a client that crashed.
- `rtc_foci` in Zendrite points at lk-jwt-service's public URL.
  When LiveKit is served at `/` of its own host, the same host can route `/sfu/get`, `/get_token` and `/delegate_delayed_leave` to lk-jwt-service.

### The Element Call app

The standalone app is a static site, published as `ghcr.io/element-hq/element-call`.
The image serves it with nginx on port 8080 as an unprivileged user and reads its configuration from `/app/config.json`.
Only the homeserver is needed there, since the app discovers the focus through `.well-known`:

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

Element Call also accepts a LiveKit setting in `config.json`, but documents it as meant for development only.
With a read-only root filesystem, the container needs writable `/tmp` and `/var/cache/nginx`.

Users of your homeserver log in to the app, start calls and share their links.
For people without an account to join by link, enable [call guests](/administration/registration/#call-guests); without them, the app's attempt to register an account for them fails while registration is closed.

### Checking the setup

```bash
# the focus Zendrite advertises
curl -s https://matrix.example.com/.well-known/matrix/client
# org.matrix.msc4140, org.matrix.msc4143 and org.matrix.msc4354 in unstable_features
curl -s https://matrix.example.com/_matrix/client/versions
# lk-jwt-service answers; without a room it returns 400 M_BAD_JSON
curl -s -X POST -H 'Content-Type: application/json' -d '{}' https://rtc.example.com/sfu/get
```

Calls that connect but carry no audio or video usually mean the media ports are blocked, most often UDP.

### Sizing

LiveKit's [benchmark](https://docs.livekit.io/home/self-hosting/benchmark/), 150 participants with video on 16 vCPUs at 85% CPU, works out to about 0.1 vCPU per participant with video.
Element Call sends video at up to 720p and 1.7 Mbit/s, so a participant uploads about 2.5 Mbit/s and downloads up to several Mbit/s, depending on how many others they see.
Audio-only calls need a small fraction of that, and LiveKit needs little memory.
A call runs on a single LiveKit node, so one node's CPU bounds the size of a call, while separate calls spread over several nodes.

## Deprecated nested configuration

Earlier Zendrite versions documented a nested spelling:

```yaml
global:
  rtc_foci:
    - type: livekit
      livekit:
        service_url: "https://matrix.example.com/livekit/jwt"
```

That form is still accepted and produces the same flat output, so existing configurations keep working.
New deployments should use `livekit_service_url`.
