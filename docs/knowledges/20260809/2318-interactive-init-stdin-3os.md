# 対話 init に stdin を 1 行渡す — 3 OS それぞれの作法 (20260809 23:18)

## Issue

waired-agent#590 の engine-only プローブは、**スイート唯一の対話 init**
（#586 のモデルピッカーに「0 = モデルを落とさない」と答えさせる）。
他のレグは全部 `--non-interactive` で、`runInitModelPicker` はその
フラグで**何も聞かずに return する**ので、ピッカーに到達できるのは
stdin に 1 行流すこの経路だけ。

Linux 版（#610）を macOS / Windows に移すとき、Linux に前例のない
問題が 2 つ出た。どちらも「試さずに書いていたら nightly を 1 周
無駄にしていた」類なので残す。

## Learnings

### 1. PowerShell には `<` リダイレクトが無い

パイプラインがバイト列ではなくオブジェクトを運ぶので、`printf '0\n' |`
の直訳が存在しない。書く前に、`GOOS=windows` でビルドした stdin 読み
取りスタブ（`bufio.Scanner` — `promptReader` がパイプ入力時に返すのと
同じもの）を WSL から実行して測った。

| 書き方 | 行が届くか | `$LASTEXITCODE` が `Tee-Object` を越えるか |
|---|---|---|
| `'0' \| & $exe` | 届く | 生きる |
| `Get-Content f \| & $exe` | 届く | 生きる |
| バッチファイル + cmd の `<` | 届く | 生きる（出力が一番きれい） |
| `& $exe`（stdin 無し・対照） | **EOF** | 生きる |

- Windows PowerShell **5.1 (5.1.26100)** と **pwsh 7.6.3** の両方で同じ結果。
  CI の Windows レグは `shell: pwsh` なので後者が本番だが、5.1 の方が
  うるさいので両方見た。
- インストール先が `C:\Program Files\...` のように**空白を含んでも変わらない**。
- 採用したのは `'0' | & $exe`。Linux の `printf '0\n' |` に一番近く、
  一時ファイルも cmd のクォート地獄も要らない。
- **対照行が重要**。stdin 無しだと EOF に落ちる ＝「黙って何も答えな
  かったレグ」の姿がこれ。だからピッカーの確認応答（`No model selected`）
  の grep が飾りではなく本命の assert になる。

### 2. `WAIRED_NO_OLLAMA` が Windows ハーネスのプロセスに残る

`installtest-windows.ps1` は `install.ps1` を `&` で呼ぶ ＝ **同一プロセス**。
その中の `Set-OllamaEnvForInit` が `$env:WAIRED_NO_OLLAMA = '1'` を置くので、
以降ハーネスが起動する `waired init` が**全部それを継承する**。

- 現状これをクリアしているのは `-DaemonEngine` 分岐だけ。エンジンを
  実際にインストールさせる必要がある唯一の他のレグだったため。
- Linux では起きない: `install.sh` が別プロセスなので変数はそこで閉じる。
- macOS でも起きない: `env "${inst_env[@]}" bash install.sh` の形で渡している。
- engine-only プローブは自分の init の前後でクリアして戻す。全体で消すと、
  その前に走る assert 群が書かれた前提が変わってしまうため。

### 3. `printf | sudo env ... | tee` の `PIPESTATUS` は `[1]`

macOS 版は既存プローブから `| tee` + `PIPESTATUS` の形を引き継いだが、
先頭に `printf` が付くのでインデックスが 1 つずれる。`[0]` のままだと
`printf` の終了ステータス（常に 0）を CLI の結果として読んでしまい、
exit-0 の assert が**絶対に落ちない**。

### 4. 実測した assert 数（run 31316424716、3 OS とも初回で緑）

| leg | executed |
|---|---|
| linux | 42 passed, 0 failed |
| macOS | 50 passed, 0 failed（`44 + 6` の導出と完全一致） |
| Windows | 75 passed, 0 failed, 0 warn |

Windows の 75 は算術で導けなかった。`-EngineOnly` は **`-Contract` 無しの
lean という初の Windows 構成**で、71（`-WithInference`）は
`Assert-Inference` の尾を数えて lean ブロックを数えず、89（`-Contract`）は
lean ブロックと contract assert を両方数えるため、差がどちらにも分解でき
ない。緑のランを 1 本作って読むしかない。

## Refs

- https://github.com/waired-ai/waired-agent/pull/615
- https://github.com/waired-ai/waired-agent/pull/610
- https://github.com/waired-ai/waired-agent/issues/590
- `scripts/dev/lib/installtest-enroll.sh` `assert_engine_only_install`
- `scripts/dev/installtest-macos.sh` `assert_engine_only_install_macos`
- `scripts/dev/installtest-windows.ps1` `Assert-EngineOnlyInstall`
