---
title: Architecture
description: How Waired connects your devices, with the four parts, enrollment through the control plane, and direct WireGuard with a relay fallback.
meta:
  audience: Anyone curious how it works underneath
  needs: Nothing
  time: 5 minutes
---

Waired is an inference-only overlay network. It introduces your computers to
each other through a control plane, then steps aside so they talk directly to
each other over an encrypted link.

## The four parts

| Component | Role |
|---|---|
| `waired` (the CLI) | What you run: `init`, `status`, `infer`, `link`, and the rest. |
| `waired-agent` | The background service (systemd, launchd, or a Windows service). It talks to the control plane and runs a userspace WireGuard data plane. |
| `waired-control` (the control plane) | A hosted service. It handles sign-in, device enrollment, and streams a signed Network Map. You do not run it. |
| `waired-relay` | A relay that forwards encrypted WireGuard packets between agents that cannot reach each other directly. |

On a computer with a screen there is also the Waired app, which talks to the
background service on the same computer and shows its state.

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

1. **Enroll.** `waired init` signs you in with Google and registers the
   device with the control plane.
2. **Discover.** The control plane streams each agent a signed Network Map:
   the public keys, endpoints, and relay URLs of the other devices on your
   network.
3. **Connect directly.** Agents open a direct WireGuard UDP link to each
   other. The data plane runs in userspace, so there is no OS-level VPN
   interface to configure.
4. **Fall back to a relay.** When two computers cannot open a direct path,
   because of strict NAT or a firewall, they fall back to a relay that
   forwards the encrypted WireGuard packets. This is automatic.
5. **Infer.** Your coding tool or client sends its request to the Local
   Gateway on this computer, which routes it to a local or peer engine and
   streams the answer back over the encrypted link.

## What the control plane does and does not do

The control plane is a coordination service. It distributes public keys and
endpoints in the signed Network Map, and that is all. It never sees your
prompts or completions, and the relay cannot decrypt the traffic it forwards.
See [Privacy: what leaves your computer](/concepts/privacy/).

## Where a turn runs

A request runs on the computer that routing chooses among your own, and a
Claude Code turn runs where the model you picked in `/model` says. Waired
never moves a turn from your computers to a cloud API on its own, or the
other way round. See [How a Claude Code turn is routed](/guides/claude-code/how-turns-are-routed/)
and [Choose which computer answers](/guides/routing/).
