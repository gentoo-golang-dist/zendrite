---
title: Troubleshooting
description: Common issues and debugging tips for Zendrite
---

If Zendrite misbehaves, work through these checks before you ask for help.

## 1. Logs

By default, Zendrite logs all warnings and errors to stdout, as well as to any other locations set in `zendrite.yaml`.
The logs often point to the cause.

To get more detail, raise the log level to `debug` in the config and restart Zendrite:

```yaml
logging:
  - type: std
    level: debug
```

Look for lines that contain `level=error` or `level=warning`.

## 2. Federation tester

If federation with other homeservers fails, check that your server passes the [Federation Tester](https://federationtester.matrix.org).

Common causes of failure:

1. Incorrect DNS configuration
2. Misconfigured SRV records or well-known files
3. Invalid TLS/SSL certificates
4. Reverse proxy configuration problems (if you use one)

Fix the errors it shows and run the tester again.

## 3. System time

Matrix depends on TLS, which needs a correct system time.
If the clock drifts, federation can become unreliable or stop working, and clients can fail to connect to Zendrite.

Make sure the system time is correct, and sync it to a reliable NTP source.

## 4. Database connections

With PostgreSQL, make sure Zendrite opens no more database connections than PostgreSQL allows.

1. In `postgresql.conf`, check the value of `max_connections` (usually `100` by default).
2. In the `global.database` section of `zendrite.yaml`, make sure `max_open_conns` doesn't exceed that number.

## 5. File descriptors

Zendrite needs a file descriptor for each open connection to a remote server, each connection to the database, and each file it reads or writes (such as media and logs).
Set the limit for Zendrite to at least 65535.

Zendrite checks the available descriptors at startup.
If there aren't enough, it logs a line like this:

```text
level=warning msg="IMPORTANT: Process file descriptor limit is currently 1024, it is recommended to raise the limit for Zendrite to at least 65535 to avoid issues"
```

Follow the [Optimisation](/administration/optimisation) instructions to raise the limit.

## 6. STUN/TURN server tester

If voice or video calls fail, check that Zendrite can connect to your TURN server with the [Matrix VoIP Tester](https://test.voip.librepush.net/).
The tester shows problems the server may run into, so you know where to start.

## 7. Delayed events with healthy probes

Zendrite processes each component's events in order, one message at a time.
One slow message, such as a state update for a room with tens of thousands of members, delays every later event, including events for unrelated rooms.
Health probes and reads of existing history keep working, so this kind of delay doesn't make the server look unhealthy.

These Prometheus metrics expose the delay.
Each is labelled with the durable consumer name, such as `ZendriteSyncAPIRoomServerConsumerPull`:

| Metric | Meaning |
| --- | --- |
| `zendrite_jetstream_consumer_processing_started_timestamp_seconds` | Unix time the consumer started its current message, or `0` while idle. |
| `zendrite_jetstream_consumer_processing_message_timestamp_seconds` | Unix publication time of the message in progress, or `0` while idle. |
| `zendrite_jetstream_consumer_processing_seconds` | Histogram of the time spent on each message. |
| `zendrite_jetstream_consumer_message_age_seconds` | Histogram of the time each message waited between publication and the start of processing. |
| `zendrite_jetstream_consumer_pending_messages` | Messages queued behind the last one delivered. |
| `zendrite_jetstream_consumer_last_ack_timestamp_seconds` | Unix time of the last acknowledged message. |

The two histograms record a message only after Zendrite finishes it.
To see a message still in progress, use the timestamp gauges.
For example, this alert fires when a consumer has spent more than a minute on one message:

```yaml
- alert: ZendriteConsumerBusy
  expr: |
    zendrite_jetstream_consumer_processing_started_timestamp_seconds > 0
    and time() - zendrite_jetstream_consumer_processing_started_timestamp_seconds > 60
```

For each room event that adds or removes at least 1000 state events, the sync API also logs `Wrote a large sync API state update` at `info` level.
The log line includes the time spent preparing and writing.
