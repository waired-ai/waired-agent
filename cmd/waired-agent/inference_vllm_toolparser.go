// #410: vLLM tool-call parser selection. vLLM populates `tool_calls`
// only when the server is started with BOTH --enable-auto-tool-choice
// and --tool-call-parser <name>; without them the model's call stays in
// `content` as prose and a coding agent pointed at the daemon gets no
// tool calls at all, for every model. The flags are emitted by
// runtime.VLLMAdapter.commandArgs; this file decides the value.
//
// Kept free of build tags — like inference_vllm_tuning.go — so the table
// is unit-tested on every platform even though only the linux vLLM path
// calls it.
//
// Why a table here rather than a field on the catalog variant (which is
// what #410 first proposed): the variant schema now lives in the
// published proto module (proto/catalog), so a field there is a wire
// contract change — its own additive-only PR, a proto tag, a
// protoconsumer exemption — for data that is not on any wire and that
// the control plane has no use for. The parser name is also vLLM's own
// CLI vocabulary, versioned with vLLM rather than with the model, which
// is a poor fit for a contract whose published versions are immutable.
// The cost of keeping it here is that a new model means editing this
// file; TestResolveVLLMToolParser_bundledCatalogCovered turns that from
// a silent omission into a failing build.
package main

import (
	"github.com/waired-ai/waired-agent/internal/catalog"
)

// Registered vLLM --tool-call-parser names. Every constant below was
// read out of the pinned engine's own registry
// (runtime.VLLMPinnedVersion = 0.24.0, `_TOOL_PARSERS_TO_REGISTER` in
// vllm/tool_parsers/__init__.py) rather than from prose, because vLLM
// validates the name at start-up and rejects an unknown one — a typo
// costs the entire engine, not just tool calling.
const (
	// vllmParserHermes is the <tool_call>{json}</tool_call> template
	// (Hermes2ProToolParser).
	vllmParserHermes = "hermes"
	// vllmParserQwen3XML is the Qwen3-Coder <function=NAME>
	// <parameter=KEY>…</parameter></function> dialect
	// (Qwen3EngineToolParser; `qwen3_coder` is a registered alias for
	// the same class).
	vllmParserQwen3XML = "qwen3_xml"
	// vllmParserOpenAI is the gpt-oss harmony format (GptOssToolParser).
	vllmParserOpenAI = "openai"
	// vllmParserGLM45 is the GLM-4.5 / 4.6 family (Glm47MoeModelToolParser).
	vllmParserGLM45 = "glm45"
	// vllmParserDeepSeekV4 is the DeepSeek V4 DSML format
	// (DeepSeekV4ToolParser).
	vllmParserDeepSeekV4 = "deepseek_v4"
)

// vllmToolParserByModelID maps a catalog model_id to the parser vLLM
// needs for that model's chat template.
//
// Every entry is evidence-backed, and the evidence is the HF repo the
// variant actually loads appearing in vLLM 0.24.0's own
// docs/features/tool_calling.md — not a guess from the model's name. A
// model whose template has not been established is deliberately ABSENT
// rather than mapped to a plausible neighbour: a wrong-but-registered
// parser makes vLLM strip or mangle the model's output, which is worse
// than the pre-#410 behaviour of passing it through as text, and the
// gateway's own recovery (#409) can still rescue the untouched text.
var vllmToolParserByModelID = map[string]string{
	// The qwen2.5-coder (hermes) and qwen3-coder (qwen3_xml) rows lived
	// here until #522 retired the 2025 generation, along with the
	// glm-4.5-air (glm45) row below. They are deleted rather than left in
	// place: TestResolveVLLMToolParser_bundledCatalogCovered iterates the
	// MANIFESTS, not this map, so an entry for a model the catalog no
	// longer ships is invisible to it — dead code that reads as coverage.
	//
	// Not in the vLLM docs table, but the dialect is measured rather
	// than assumed: #409 recorded the Qwen3.5/3.6 lineage emitting the
	// same <function=…> XML (and ollama's strict encoding/xml parser
	// 500ing on it), which is exactly what qwen3_xml consumes.
	"qwen3.6-27b": vllmParserQwen3XML, // Qwen/Qwen3.6-27B
	// Same dialect, and read from the artifact rather than inherited by
	// family name: Qwen/Qwen3.8-27B's chat_template.jinja differs from
	// Qwen/Qwen3.6-27B's overall (it grew a multimodal branch), but the
	// tool-call markup it emits is byte-identical —
	// `<tool_call>\n<function=NAME>\n…</function>\n</tool_call>`, with
	// `<parameter=…>` inside. That is what qwen3_xml consumes (#823).
	"qwen3.8-27b": vllmParserQwen3XML, // Qwen/Qwen3.8-27B

	// docs §"OpenAI OSS Models (`openai`)", both repos listed by name.
	"gpt-oss-20b":  vllmParserOpenAI, // openai/gpt-oss-20b
	"gpt-oss-120b": vllmParserOpenAI, // openai/gpt-oss-120b

	// No docs section, but vLLM registers `deepseek_v4` and its
	// DeepSeekV4ToolParser is documented in-source as "DeepSeek V4 DSML
	// tool parser" — the same major version this variant loads.
	"deepseek-v4-flash": vllmParserDeepSeekV4, // deepseek-ai/DeepSeek-V4-Flash
}

// resolveVLLMToolParser returns the value for vLLM's --tool-call-parser
// when serving m, or "" to emit neither tool-calling flag.
//
// A non-empty override (inference.vllm_tool_parser) always wins,
// verbatim and unvalidated: it exists so an operator can reach a parser
// vLLM registered after this binary was built, which a whitelist here
// would defeat. The cost is that a typo in it fails engine start-up —
// stated on the config field.
func resolveVLLMToolParser(m catalog.Manifest, override string) string {
	if override != "" {
		return override
	}
	return vllmToolParserByModelID[m.ModelID]
}
