---
status: accepted
superseded_by:
  - docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md
---

# モデル名を書かない要求では、ルータはモデルではなくノードを選ぶ (20260819 19:00)

## Status

Accepted。オーナー裁定（2026-08-19、waired-ai/waired#1227 レーン L63 の着手前確認）。

**§4 の前提のみ、2026-08-28 の裁定が引き継いだ**
（`docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md`）。
§4 の機構 — `ResolveUnknownModel` は `router.DefaultModelAlias` を返すだけ — は
そのまま生きている。変わったのは「Claude Code が送る Anthropic id は指名なしに翻訳する」
という読み方のほうで、いまは実 Anthropic の id が実行先の指定として扱われる。この §4 が
効くのは、`/model` が Waired の行を指しているセッションだけになった。
§1〜§3 と §5、および pin 裁定 #1 の維持は影響を受けない。
2026-05-19 の worker pin 裁定のうち **#2（pin 先が要求 alias を serve していない →
別 peer へ soft fallback）を改定**する。#1（pin 先 unreachable → strict 503）は維持。

## Context

`waired/default` は routing mode を見る**前**に `Inputs.DefaultModelID`
（preferred > 永続 active > bundled — すべてリクエスト元のローカル事実）へ解決され、
メッシュ候補はその 1 モデルの広告者だけと突き合わされていた
（`resolveModel` → `variantWantSets` → `buildMeshCandidates`）。
1 agent = 1 モデル広告（docs/decisions/20260811/2340-one-model-resident-at-a-time.md）
なので、**pin も peer-only も両端が同じモデルを載せている時しか動かない**。

v0.0.3-rc2 のオーナー実機レビュー（waired-ai/waired#1223、4 ノード）では、
これが全 OS 対で同じ形で出た。macOS（active = qwen3.5-35b-a3b）から
Linux GPU 機（qwen3.8-27b）へ tray で pin した状態:

```
❯ waired infer "fugafuga"
waired: gateway returned 404: {"error":{"message":"router: model is not in ready state on disk: \"qwen3.5-35b-a3b\" state=\"ready\" (routing=pinned, no mesh candidate)","type":"invalid_request_error","code":"model_not_served"}}
```

エラーが名指ししているのは **リクエスト元のモデル**で、`state="ready"` と
「not in ready state」が同じ行で矛盾している（メッシュ枝が local 枝の文を借りていた）。
一方で `--explain --model <相手のモデル>` は正しく `execution: remote` を選ぶ。
ルータは健全で、**モデル名が routing の鍵になっていること**だけが問題だった。

歴代のメッシュ試験がすべて `--model <相手の active>` を明示していたため
（モデル混在は「リモート経路を強制する道具」として使われていた）、
素の `infer` + pin というユーザーの実動線は一度も試験されていなかった
（waired-ai/waired#1226 §6）。

## Decision

**モデル名を書かない要求（`waired/default`、および Claude Code が送る Anthropic id）
では、routing mode がノードを選び、そのノードが載せているモデルで推論する。**

### 1. pin はノードへの指定であって、(node, model) の組ではない

pin 先が要求と違うモデルを載せていても、**その pin で推論する**。
相手が意図していないモデルを動かしていたとしても、推論はそのまま通す（オーナー裁定）。
2026-05-19 の #2 が与えた「別 peer へ soft fallback」は、pin の意味を
(node, model) 寄りに引き戻していたので改定する。

置換は無言にしない: 選択理由に
`pinned peer %q is serving %q; a pin names a node, ...` を残し、
応答の `model` はエンジン自身が答えた名前になる。

soft fallback が残るのは **pin がカタログの知らないモデルを広告している**場合だけ。
そこには推論に使えるモデルが無いので、別 peer に回す以外の答えが無い。

### 2. peer の自動選択（peer-only / peer-preferred / auto）でもモデルは鍵にしない

未指定の要求では、want set をカタログ全体の和集合にする（`wantSetsFor`）。
ノード側のフィルタはすべてモデル非依存のまま効く:

- 到達可能性 / stale
- `MinContextWindow`（waired#1031 の帯 = 「その帯を約束するノードか」）
- Claude クラスの `ExcludeMain` / `ExcludeSub`
- Public Share の admits と own > public の分割
- admission（capacity）・sticky・RTT・エラー率・負荷率の順序付け

**クラス分けはあってもモデルは厳密に選ばない**、というのがこれらの層の元々の設計で、
モデル一致という追加の鍵だけが噛み合っていなかった。

### 3. モデル名を書いた要求は、pin が無い限り厳密一致のまま

`--model` は「このモデルを使う」という選択の手段として残す。
pin という寄る辺が無い状態で別のモデルに振り替えるのは推測になるので、しない。

### 4. Claude 面は「指名なし」に翻訳する

Claude Code が送る Anthropic id はカタログに存在しないし、存在させる意図も無い
（ユーザーが `/model` で選んだのは帯であってこのフリートのモデルではない）。
`ResolveUnknownModel` は **`router.DefaultModelAlias` を返すだけ**にする。
pin 先のモデルやデバイス active モデルへ解決していた実装は、
「リクエスト元は何のつもりか」という、今回捨てた問いへの答えだった。

### 5. メッシュ枝のエラーは、メッシュの不足を名指しする

`ModelNotReadyError.Mesh` を立て、文を書き分ける。センチネルは
`ErrModelNotReady` のままなので、gateway / management のマッピングは不変。

- 旧: `router: model is not in ready state on disk: "X" state="ready" (routing=pinned, no mesh candidate)`
- 新: `router: no mesh peer serves "X" (routing=pinned); local state="ready"`
- 指名なしの要求: `router: no mesh peer is available (routing=peer-only); local state="ready"`

## Consequences

- **pin の動機（「ノートから GPU 機を使う」）が素の `waired infer` で通る。**
  混在フリートで両端のモデルを揃える必要が無くなる。
- **エンジンを持たないノードが Claude Code を含めてメッシュへ出られる**
  （ローカル実行スコープへゲートを縮めた waired-agent#829 と対で効く）。
- `router.ResolveModelForPeer`（#647）は呼び出し元が無くなったので削除した。
  「そのノードは何を serve しているか」はカタログ全体の want set と
  `buildMeshCandidates` が答えるようになり、専用の解決経路が要らない。
- **pin 先が想定外に弱いモデルを載せていると、そのモデルで答える。**
  ノード指定の帰結として受け入れる（オーナー裁定）。帯を守る必要がある要求は
  `MinContextWindow` が引き続き硬いフィルタとして効く。
- 未指定要求のピア順序は、モデル強度（ParamCount × QuantizationTier）を含む
  既存のスコアで決まる。1 つの engine 識別子を 2 つの manifest が主張した場合は
  同じスコアで強い方 → ModelID の順に決定的に解決するので、フリート内で
  ホストごとに答えが割れることはない。

## References

- waired-ai/waired-agent#828（本件）/ waired-ai/waired-agent#829（対のゲート修正）
- waired-ai/waired#1223（オーナー実機レビュー）/ waired-ai/waired#1226 §2, §6（機構と見逃しの経緯）
- waired-ai/waired#1227 レーン L63
- docs/decisions/20260811/2340-one-model-resident-at-a-time.md（1 agent = 1 モデル広告）
- 2026-05-19 の worker pin 実装記録（private 側）— #1 は維持、#2 を本決定が置き換える
