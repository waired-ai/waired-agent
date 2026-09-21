# コーディングクライアントの統合で読み違えやすい事実 (20260921 21:55)

## Issue

Claude Code を Waired につなぐコード（`internal/integration/claudecode`、
`internal/integration/claudemanaged`、`internal/gateway`）を触るときに、
クライアントの側の挙動として知っておく事実を 3 つの主題にまとめた。
実測と機構の大半は既存の note とコードのコメントに在るので、そこはパスを指し、
書いていない点（読み違えた点、気づきにくい理由、確かめる手順）だけを書いている。
出自: セッションのメモリに在った知見を、オーナーの指示（2026-09-21、
waired-ai/waired-ai#12）でリポジトリへ移した。

## Learnings

### 1. Claude Code はセッションのコンテキストウィンドウを model id の綴りだけで決める

いつ読むか: `/model` に出す行の id を変えるとき、managed settings の `env` を変えるとき、
「この行のセッションは何トークンか」を調べるとき。

| 条件 | セッションのコンテキストウィンドウ | カタログ外の id の注意書き |
|---|---|---|
| id に `[1m]` を含む（大文字小文字を区別しない。位置はどこでもよい） | 1,000,000 | 出ない |
| `claude-` で始まらない id で、`CLAUDE_CODE_MAX_CONTEXT_TOKENS` > 0 | その環境変数の値 | 出ない |
| それ以外 | 定数 200,000 | 出る |

実測の正本は `docs/knowledges/20260906/0340-the-model-picker-measured-again.md` §2〜§4
（述語の逐語、4 条件の表、注意書きの全文、`[1m]` が環境変数に勝つこと）。
製品がこの規則にどう合わせているかは
`internal/integration/claudemanaged/managedsettings.go`（書く値は 200704。オーナー決定
2026-09-16、waired-agent#1396）と `internal/gateway/anthropic_models.go` のコメントに在る。
分岐の順序と再測定の手順を含む詳しい記録は、内部の記録を参照。

- 環境変数は 1 本の global で、`claude-` で始まらない id の全部が共有する。
  コンピュータごと・モデルごとの値は、この仕組みでは表せない。

#### `[1m]` はワイヤで剥がれる

`--model claude-waired-cloud[1m]` のリクエスト body は `model: claude-waired-cloud` になり、
1M の要求は `anthropic-beta: context-1m-…` ヘッダにしか残らない（Claude Code 2.1.229 /
2.1.241 で実測。2.1.245 と 2.1.261 でも同じ）。正本は
`docs/knowledges/20260828/0300-what-claude-code-puts-on-the-wire.md` §1 と
`internal/integration/claudecode/directives.go` の `TierMarker1M`。

- だから製品の側は、id を `[1m]` を剥いだ形で比べ、1M の要求はヘッダからも読む
  （`internal/gateway/anthropic_models.go` の `RequiredWindowForRequest`）。
- `[1m]` 付きの id を実機で確かめるには、ワイヤの捕捉が要る。画面と
  `/context` だけでは、body に何が乗ったかは分からない。

#### 読み違えた 2 点（どちらも実測で覆った）

- `modelOverrides` は、コンテキストウィンドウを宣言する仕組みではない。
  「Anthropic の model id → プロバイダ固有の id」の対応表で、キーは実在するモデルで
  なければならない。合成した id に使おうとすると、実在する model id を 1 つ占有し、
  そのモデルを選んだ人のリクエストまで Waired に来る。
- `CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1` は、注意書きの文言だけを
  消す設定ではない。コンテキストウィンドウの数値は 200000 のまま、その出どころが
  `"auto"` になり、自動コンパクションをするかどうかの判定が false を返すときと同じ
  条件になる。つまり長いセッションが、コンパクションされないまま何も言わずに失敗する。

#### 測るときに踏んだ点

- PowerShell の `Out-String` は端末の幅で折り返す。画面の文言と照合するなら、
  raw で採って `-replace '\s+',' '` してから比べる。そうしないと、出ている文言を
  「出ていない」と判定する。
- managed settings の `env` は、プロセスの環境変数に勝つ。managed settings が同じキーを
  持っているコンピュータでは、プロセスの側で環境変数を振っても効かない。
- `modelOverrides` を試したときは、プロジェクトスコープの `.claude/settings.json` に置いた。
- Claude Code のバイナリは `strings` で読める。`grep -aob '<文言>'` でオフセットを採り、
  `dd` で周辺を切り出して `tr -c '[:print:]\n' '\n'` に通すと、該当の関数が読める。

### 2. hook / statusLine の `command` は、OS ごとに別のシェルに渡る

いつ読むか: Waired が Claude Code の設定に書き込む `command` の文字列を変えるとき、
Windows 向けの文字列をレビューするとき。

一次情報は https://code.claude.com/docs/en/hooks と
https://code.claude.com/docs/en/statusline（2026-08-15 に確認し、2026-09-21 に
同じ記述であることを読み直した）。

- hooks の shell 形式: macOS / Linux は `sh -c`、Windows は Git Bash、
  Git Bash が無ければ PowerShell。
- statusLine も同じ解決の規則で、`shell` フィールドも exec 形式も無い。
  だから Windows 用の 1 本の文字列が、Git Bash と PowerShell の両方で動く必要がある。

製品がこれにどう合わせているかは、コードのコメントが正本:
`internal/integration/claudemanaged/hook.go` の `fallbackHookCommandFor` /
`hookCommandFor`、`internal/integration/claudecode/statusline.go` の
`statuslineRenderCommandFor` / `statuslineWrapperCommandFor`
（パスはフォワードスラッシュで常にダブルクォート、`powershell.exe -NoProfile
-ExecutionPolicy Bypass -File "..."`、`filepath.ToSlash` ではなく `strings.ReplaceAll`）。
固定しているテストは `internal/integration/claudecode/statusline_shell_test.go`
（waired-agent#787）。

コメントに無い点:

- hooks には `args`（exec 形式。シェルを通らない）と `shell: "bash" | "powershell"` が在るが、
  対応する Claude Code の版はドキュメントに書かれていない。managed-settings.json は
  コンピュータ全体に効くので、対応していない版がそのフィールドを弾くと、hook ごと壊れる。
  採用するなら、その前に実機で確かめることになる。
- 既定の ExecutionPolicy が `-File` の未署名 .ps1 を拒むのは、Claude Code からの実行に
  限らない。WSL から `pwsh.exe -File` で自作のスクリプトを走らせても、同じ拒否が出る。
- 「POSIX の構文だから Windows では動かない」は、Git Bash が入っているホストでは
  成り立たない。この形の断定を見たら、Git Bash の有無を先に確かめる。
  逆に、Git Bash が入っている開発用のホストで動いたことは、Git Bash の無いホストで
  動く証拠にならない。

### 3. Windows の PATH 上の `bash.exe` は WSL のランチャー

いつ読むか: Windows で bash を探すコードを書く・レビューするとき。

```
C:\WINDOWS\system32\bash.exe                                  ← WSL のランチャー
C:\Users\<user>\AppData\Local\Microsoft\WindowsApps\bash.exe  ← WSL のストアのエイリアス
```

Git for Windows は、入っていても自分を PATH に載せない
（`C:\Program Files\Git\bin\bash.exe` は実在するのに、`Get-Command bash.exe -All` に
出てこない）。だから `Get-Command bash.exe` で Git Bash を探すコードは、
Git Bash が在るコンピュータで WSL を掴む。Git Bash 用に書かれた statusLine を
WSL に渡すと、別のファイルシステムの名前空間で走って失敗し、利用者自身の
statusLine の出力が何も言わずに消える（waired-agent#816）。

Git Bash は設置場所で探す。実装と理由は
`internal/integration/claudecode/statusline.go` の `wrapperScriptPS1` に在る:

- `git.exe` の親の親 + `bin\bash.exe`（`git.exe` は PATH に載るので、これが一番当たる）
- `%ProgramFiles%\Git\bin\bash.exe` / `%ProgramFiles(x86)%\...` /
  `%LOCALAPPDATA%\Programs\Git\bin\bash.exe`
- 見つからなければ PowerShell にフォールバックする。その名前に応答する何かには渡さない。

コメントに無い点:

- 気づきにくい理由: Git Bash から走らせると、正しく動いて見える。Git Bash は自分の
  `bin` を子プロセスの PATH の先頭に置くので、同じ `Get-Command` が Git Bash に解決される。
  一方、Claude Code が PowerShell を選ぶのは Git Bash が無いとき、つまり PATH 上に
  WSL の `bash.exe` しか無い場合である。手当ての要る側でだけ壊れる。
- ユニットテストでは出ない。wrapper が Go の文字列定数だと、テストが assert できるのは
  その内容だけで、PATH の解決の差は、WSL の入った実際の Windows のホストでしか出ない。
- 生成される `.ps1` は、WSL から `powershell.exe` と `pwsh.exe` を呼べる開発環境なら、
  バイナリを差し替えずに確かめられる: Go のテストで定数をファイルに書き出し
  （環境変数で有効にする使い捨てのテスト。PR の前に削除する）、`powershell.exe`（5.1）と
  `pwsh.exe` の両方で AST をパースし、実行する。書き出したファイルを検証用の
  Windows のホストへ `scp` で置けば、実際の PATH の下での挙動も確かめられる。

## Refs

- waired-ai/waired-ai#12（メモリからリポジトリへ移す指示）
- https://github.com/waired-ai/waired-agent/issues/787
- https://github.com/waired-ai/waired-agent/issues/816
- https://github.com/waired-ai/waired-agent/issues/1036
- https://github.com/waired-ai/waired-agent/issues/1396
- https://code.claude.com/docs/en/hooks
- https://code.claude.com/docs/en/statusline
- docs/knowledges/20260828/0300-what-claude-code-puts-on-the-wire.md
- docs/knowledges/20260906/0340-the-model-picker-measured-again.md
- internal/integration/claudecode/statusline.go, internal/integration/claudecode/statusline_shell_test.go
- internal/integration/claudecode/directives.go
- internal/integration/claudemanaged/hook.go, internal/integration/claudemanaged/managedsettings.go
- internal/gateway/anthropic_models.go
