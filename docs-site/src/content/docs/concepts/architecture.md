---
title: Architecture
description: How Waired connects your devices — the four binaries, enrollment via the control plane, and direct WireGuard with relay fallback.
meta:
  audience: Anyone curious how it works underneath
  needs: Nothing
  time: 10 minutes
---

Waired is an **inference-only overlay network**. It introduces your machines to
each other through a coordination service, then gets out of the way so they talk
directly, peer-to-peer, over an encrypted link.

## The four parts

| Component | Role |
|---|---|
| `waired` (CLI) | What you run: `init`, `status`, `infer`, `link`, and the rest. |
| `waired-agent` | The background service (systemd / launchd / Windows service). It talks to the control plane and runs a userspace WireGuard data plane. |
| `waired-control` (Control Plane) | A hosted service. It handles Google sign-in, device enrollment, and streams a signed **Network Map**. You don't run it. |
| `waired-relay` | A DERP-style relay that forwards encrypted WireGuard packets between agents that can't reach each other directly. |

## How a request flows

```
+-----------+   OAuth + enroll    +-------------------+
|  waired   | ──────────────────► |  waired-control   |
|  (CLI)    |                     |  (Control Plane)  |
+-----------+                     +---------+---------+
      │                                     │  signed Network Map
      ▼                                     ▼  (peer keys + endpoints + relay URLs)
+-----------+   WireGuard direct UDP   +-----------+
| waired-   | ◄──────────────────────► | waired-   |
|  agent A  |                          |  agent B  |
+-----+-----+                          +-----+-----+
      │            +---------------+         │
      └───wss────► | waired-relay  | ◄──wss──┘
                   +---------------+
                forwards encrypted WG packets only
```

1. **Enroll.** `waired init` signs you in with Google and registers the device
   with the control plane.
2. **Discover.** The control plane streams each agent a CP-signed Network Map —
   the public keys, endpoints, and [relay](/reference/glossary/#relay) URLs of the other devices on your
   network.
3. **Connect directly.** Agents open a direct WireGuard UDP link to each other.
   The data plane runs in userspace, so there's no OS-level VPN interface to
   configure.
4. **Fall back to relay.** When two boxes can't open a direct path (strict NAT
   or firewall), they fall back to a relay that forwards the *encrypted*
   WireGuard datagrams. This is automatic.
5. **Infer.** Your [coding agent](/reference/glossary/#coding-agent) or client hits the [Local Gateway](/guides/chat-clients/),
   which routes the request to a local or peer engine and streams the answer
   back over the encrypted link.

## What the control plane does and doesn't do

The control plane is a *coordination* service. It distributes peer public keys
and endpoints via the signed Network Map — and that's all. It never sees your
prompts or completions, and the relay can't decrypt the traffic it forwards.
More on that in [Privacy](/concepts/privacy/).
