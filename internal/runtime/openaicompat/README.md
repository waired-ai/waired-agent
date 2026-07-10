# openaicompat — external OpenAI-compatible adapter

This package is the `runtime.Adapter` for an HTTP endpoint the agent
does **not** own — typically a vLLM / LM Studio / TGI server running
elsewhere on the LAN, or even a paid hosted OpenAI-compatible API.
The adapter is registered under `openai-compat:<id>` and selected by
the router's Phase 5 external-fallback branch when no local engine has
the requested model.

## When to use it

Add an entry when this host:

- has no GPU to run vLLM locally
- can reach a colleague's GPU box at a stable URL
- wants the waired gateway to transparently route `claude` /
  `opencode` requests through that GPU's vLLM instead of falling
  back to api.anthropic.com

## Config

`~/.config/waired/agent.json`:

```json
{
  "inference": {
    "external_endpoints": [
      {
        "id": "lan-vllm",
        "url": "http://192.168.1.10:8000/v1",
        "auth_env_var": "LAN_VLLM_KEY"
      },
      {
        "id": "openai",
        "url": "https://api.openai.com/v1",
        "auth_env_var": "OPENAI_API_KEY"
      }
    ]
  }
}
```

`id` is a free-form label used as the registry suffix
(`openai-compat:lan-vllm`). `url` accepts both `http://host:port` and
`http://host:port/v1`. `auth_env_var` is optional — when set, the
named env var's value is sent as `Authorization: Bearer <value>` on
every outbound request.

The systemd unit (or whatever runs `waired-agent`) is responsible for
exporting `LAN_VLLM_KEY` / `OPENAI_API_KEY` into the agent's
environment. The token is captured **once at agent boot** so a `kill
-HUP`-style restart is required after rotating it.

## Scope (Phase 5)

Agent-local only. The adapter is never advertised in
`signer.InferenceState`, so mesh peers never see the endpoint and
cannot proxy through this agent to reach it. This means:

- the operator's Bearer token never leaves this host
- a peer's WG-arriving request that triggers this agent's overlay
  Selector will **not** be funnelled out through the external endpoint
  (loop prevention via `Inputs.AllowExternal=false`)

Mesh advertisement (`advertise_to_mesh: true`) is a Phase 6+ feature.
