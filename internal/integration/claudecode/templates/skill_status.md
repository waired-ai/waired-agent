---
name: waired-status
description: Show Waired status and summarize whether local inference is ready for coding-agent traffic.
allowed-tools: Bash(waired status:*), Bash(waired status)
---

Run `waired status`. Read the output and summarize for the user, in two
or three sentences, whether Waired is ready to serve coding-agent
inference requests right now.

Mention:
- Whether the local Gateway is running.
- Which model is loaded (or "no model loaded").
- Whether the device is connected to its Waired Network.

If anything looks wrong, suggest the user run `/waired-doctor`.
