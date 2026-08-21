---
status: accepted
---

# 秘密は 3 OS すべてで 0600 ファイルに置く (20260822 03:57)

## Status
Accepted

## Context

エージェントの秘密 (Machine Key・access token・refresh token・gateway
token) は、これまで 2 か所に書かれていた。**権威はファイル** —
`internal/platform/secrets` が 0700 のディレクトリに 0600 で原子的に書く
(Windows は NTFS DACL) — で、その上に **macOS だけ Keychain のミラー**が
乗っていた (`internal/platform/securestore` が両者を合成し、読みは
Keychain 優先)。Keychain 側は at-rest の強化層という位置づけだった。

実測 (2 台の Mac、2026-08-21〜22) で、その位置づけが成り立っていないことが
分かった。要点だけ挙げる (詳細は private の waired リポの決定記録):

- root で走るデーモンは共有の System keychain に書く。そこに置いた項目は、
  **同じ Mac の他のローカルアカウントから読める**。一方ファイル側の
  `/Library/Application Support/waired` は `drwx------ root:admin` で、
  非 root は一覧すらできない。**強化層のほうが、強化する対象より弱かった。**
- そのうえ書き込みは安定して成功していなかった。access token と refresh
  token は数十分ごとに回るが、更新のたびに失敗し、`securestore` の
  never-stale 掃除が項目を消す。実機 2 台ともこの 2 つは keychain に不在。
- System keychain は人間の秘密なしに開く (コンソールに誰もログインして
  いないヘッドレス Mac で読める) ので、正しく動いたとしても、ディスク全体の
  暗号化が与える以上の at-rest 保護にはならない。

ユーザー権限側の login keychain も実測で空だった。非 root の読みは検索
リスト経由で root の System keychain を先に見つけるため、ユーザー側への
移行書き込みが一度も発火していなかった。

## Decision

**Keychain の層をやめ、3 OS すべてで「0700 ディレクトリの中の 0600
ファイル」を唯一の保管先にする。**(オーナー裁定、2026-08-22)

`internal/platform/keychain` と `internal/platform/securestore` を削除し、
呼び出し元は `internal/platform/secrets` と `os.ReadFile` /
`os.Remove` を直接使う。`internal/platform/secrets` の
`linux || darwin` / `windows` 分割は残る — これはモードビットと NTFS DACL の
違いによるもので、Keychain とは無関係。

**at-rest の保護はディスク全体の暗号化 (FileVault / BitLocker / LUKS) に
依存する**、と明示的に置く。ファイル権限が守るのは「同じホストの別の
ローカルユーザー」までで、取り外されたディスクは守らない。これは 3 OS
共通の性質で、リリース前の現時点で受け入れる。

## Consequences

- **macOS が他の 2 OS と同じ挙動になる。** state dir を消せば Machine Key
  も消え、そのホストは新規デバイスとして enroll する。これまで macOS だけ
  Keychain のミラーが state dir より長生きし、`--clean` 後の再インストールが
  同じデバイス行に戻っていた (#680、waired#1136)。
- **消える機構**: `uninstall.sh` の macOS 限定ステップ (`waired logout` を
  root とユーザーで二度呼んで Keychain 項目を消していた)、CI の
  `unit tests (darwin, seeded host)` レグでの Keychain seeding、
  `securestore.SwapStoreForTest` とそれを設置するためだけに在った 6 つの
  `seams_test.go`、`sudo waired init` が darwin でだけ組んでいた
  `launchctl asuser <uid>` の二段ホップ (#799 — securityd のセッション
  エージェントに届かせるのが唯一の目的だった)。
- **反転したテスト 3 本**。「ファイルを消しても Keychain から戻る」を主張して
  いたものを、「ファイルを消したら戻らない」に置き換えた
  (`TestLoadOrCreateMachineKey_FileLossLosesTheKey` /
  `TestAccessToken_FileLossReadsAsAbsent` /
  `TestLoadOrCreateGatewayToken_AlwaysLeavesTheFile`)。旧実装ではどれも
  逆向きに緑だったので、判別力があることは確認済み。
- **`installtest-macos.sh` の #680 の回帰バーは向きを変えて残す**。
  「`--clean` の後に Machine Key が残っていないこと」から
  「そもそも System keychain に自分の項目が 1 つも無いこと」へ。hosted
  runner は空の System keychain で始まるので、これは「誰かが Keychain 書き
  込みを復活させたら赤くなる」ガードとして機能する。
- **リリース前なので移行は行わない**。既存ホストに残る項目は、対応する
  デバイス行が revoke され、読むコードも無くなるため不活性になる。手元の
  検証機は現行ビルドのまま `uninstall.sh --clean` を流して消す。

## Refs
- waired-agent#799 / #680 / #654 / #261 / #512 / #520
- docs/knowledges/20260821/2230-security-cli-reports-twice.md
- 実測と脅威モデルの詳細は private の waired リポの決定記録 (内部ノート参照)
