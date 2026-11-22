---
title: Enabling presence
description: Configure online and offline presence status in Zendrite.
---

Presence shares your users' online and offline status with other users and receives their status in return.
Supported clients display it.

Presence is disabled by default because it costs performance.
It uses more CPU time and makes your server send more federation traffic.

Set the two directions in the `presence` block of the `global` section:

```yaml
global:
  # ...
  presence:
    enable_inbound: false
    enable_outbound: false
```

- `enable_inbound`: Zendrite handles presence updates for remote users and passes them to your local users.
- `enable_outbound`: Zendrite generates presence updates for your local users and sends them to remote users over federation.

You can enable one direction only.
For example, enable inbound presence to receive presence from other servers without revealing the presence of your own users.
