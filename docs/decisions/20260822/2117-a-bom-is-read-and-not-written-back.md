---
status: accepted
---

# openclaw.json の UTF-8 BOM は読み飛ばし、書き戻さない (20260822 21:17)

## Status
Accepted

## Context

`readConfigObject` は `os.ReadFile` の結果をそのまま `json.Unmarshal` に
渡していた。`encoding/json` は先頭の `U+FEFF` を受け付けないので、
UTF-8 BOM の付いた `openclaw.json` は

```
openclaw: parse C:\Users\…\.openclaw\openclaw.json: invalid character 'ï' looking for beginning of value
```

で落ちる。この読み口は3か所が使っている: `Apply`(link と tray の
Reconfigure)、`Uninstall`(**unlink も出来なくなる**)、`auditConfig`
(`waired doctor` の行)。

**OpenClaw 自身は同じファイルを読める**(実測: BOM を付けた状態で
`openclaw plugins list` が waired プラグインを `enabled` と表示した)。
つまり、ファイルの持ち主である製品は受け入れるのに waired だけが拒む。

**Windows で最も踏みやすい**。PowerShell 5.1 の
`Set-Content -Encoding utf8` は BOM を書き、Windows のエディタにも既定で
付けるものがある。実際、#995 の実機検証で**自分の検証スクリプトが**
これを書いてしまい、以後そのホストの link が全部落ちた。エディタで見ても
ファイルは何も変わって見えず、エラーはそこに存在しない `'ï'` を名指しする。

## Decision

**読むときに BOM を落とし、書き戻さない。**

- `readConfigObject` が `bytes.TrimPrefix(data, utf8BOM)` してから parse し、
  **返す生バイトも落とした後のもの**にする。結果として、BOM 付きの設定は
  Apply の比較(20260822/2030)で「変わっている」と判定され、**1回だけ**
  バックアップを取って BOM 無しで書き直され、以後は収束する。
- **BOM は保存しない。** `marshalConfig` は既にユーザーのキー順と整形を
  落としている。符号だけ残すのは一貫しないし、保存するには読みから書きまで
  フラグを運ぶ必要がある。**変更前に取るバックアップが、BOM ごと元の
  ファイルを保持している**ので、失われる情報は無い。
- parse に失敗したときの文言を、生の `encoding/json` から
  **どのファイルか / waired は変更していない / どうすればよいか**を言う形に
  変える。ユーザーが所有するファイルの失敗なので、CLI の他の失敗と同じ
  基準を満たすべきである。

## Consequences

- BOM 付きのホストでは、次の link が設定を1回だけ書き直す(バックアップ1本)。
  以後は #995 の規則どおり無変更なら何も書かない。
- `configHasForeignKeys` は parse エラーを「実ユーザーの内容」と見なす作りに
  なっている(waired#753 の自己汚染回避)。BOM がその判定を**誤った理由で**
  発火させていたのも同時に消える。
- BOM を出力に混ぜる経路は元から無い。`writeFileAtomic` が書くのは
  `marshalConfig` の結果だけ。
- opencode / claude-code のアダプタはユーザー所有の JSON を parse しないので
  対象外(OpenCode のプラグインは waired が全所有する JS、VSCode 系の読み口は
  producer の無い旧台帳専用)。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1002
- docs/decisions/20260822/2030-integration-backs-up-only-a-real-change.md
- https://github.com/waired-ai/waired/issues/753 (設定ディレクトリの自己汚染回避)
