# ローテートするログと、1 行の面 (20260829 15:30)

## Issue

#1112 / #1111 / #1137 を直す過程で、別々に見えて同じ形をした穴が 2 つ出た。
どちらも「その面が何を運べるか」を書き手が確かめずに渡していた。

## Learnings

### 1. `.log` だけ採ると、一番情報の無い世代が残る

`openEngineLog` は **spawn ごとに** `engine.log` → `engine.log.1` へリネームする
(`internal/runtime/ollama.go`)。アダプタはクラッシュしたエンジンを自動で
再起動するので、**この収集が存在する理由そのものの障害(クラッシュループ)では、
最初の・情報のある試行はすでに `.1` 側へ回っており、残っている `engine.log` は
最後の一番情報の無い試行**になる。

CI の収集ステップは 3 つあり、いずれも `.1` を採っていなかった。製品側で
正しくやっているのは `internal/platform/logdump` だけで、そこは
`*.log.1` を先に採る。

**収集ステップが 3 つ在ったこと自体が原因**でもある。書かれた時期が違い、
それぞれ別のものを忘れていた(Windows の 2 レグは収集ゼロ、Linux の
install+inference も収集ゼロ、agent 自身のログを採っていたのは Windows の
daemon-path だけ)。1 本のスクリプトに寄せると、この型は再発しない。
`shell: bash` は **windows-latest では Git Bash** なので、3 OS で 1 本にできる
(同じワークフローの job summary が既にそうしている)。

### 2. 1 行の面に渡す前に絞る規則が、パッケージによって在ったり無かったり

`last_error` は give-up の一文 + 生の失敗 + **最大 4 KiB の engine.log 末尾**。

- `cmd/waired-agent` には規則が在る:`servingFailureReason` は `firstLine` を通し、
  その doc が理由まで書いている(「呼び出し元が 1 行の面だから」)。
  setup のワイヤは `clampSetupDetail`。
- `internal/gui/tray` には**無かった**。メニュー項目のタイトルに生で渡っており、
  Linux では `escapeMenuLabel` がログ末尾全体の `_` を `__` に倍化していた。

**clamp 幅は測って決めること。** 160 runes だと原因は載るが**remediation が切れた**
(「set inference.vllm_port in agent.json to a free port」の側)。人が行動する
半分が消えるので、製品が**この行のために書いた最長の文**(busy-port の refusal、
約 197 runes)に合わせて 240 にし、テストで丸ごと固定した。
上限の役目は「4 KiB のログを行にしない」ことであって、文を短くすることではない。

### 3. 複数行を `grep -F` のパターンにしない

重複排除で「この赤いレーンの集合はもう報告済みか」を
`grep -qF -- "$RED_LANES"` で見ようとして間違えた。`-F` の複数行パターンは
**複数のパターン**で、そのうち 1 つが**空文字列 = 何にでも一致**する。
結果、あらゆる障害が「報告済み」になった。自己テストが捕まえた。
1 行の marker(`<!-- red-lanes: a;b -->`)を比較する形に変えた。

あわせて、`existing="$(… | grep …)"` は `set -e` 下で**一致ゼロのとき
スクリプトごと落ちる**(代入の中のパイプラインが失敗するため)。`|| true` が要る。

## Refs
- https://github.com/waired-ai/waired-agent/pull/1142
- https://github.com/waired-ai/waired-agent/pull/1146
- https://github.com/waired-ai/waired-agent/issues/1112
- https://github.com/waired-ai/waired-agent/issues/1137
