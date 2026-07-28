---
status: accepted
---

# GNOME トレイホスト拡張は、パッケージ依存ではなく実行時判定で導入する (20260728 15:25)

## Status
Accepted

## Context

GNOME は StatusNotifierItem ホストを持たないため、AppIndicator ホスト拡張が入って
いないと waired-tray のアイコンは**無言で表示されない**。ユーザーから見える不具合の
報告経路がアイコンそのものなので、気づく手段がない。

これを導入していたのは `cmd/waired/init_tray_linux.go` の `ensureTrayHostExtension`
だったが、唯一の呼び出し元が standalone enrollment の末尾で、実インストールは全て
daemon 経由だったため**既に到達不能**だった。#301 でファイルごと削除され、残ったのは
`packaging/nfpm/waired-tray.yaml.tmpl` の `suggests:` のみ。apt は Suggests を自動
導入しないので、GNOME ホストには誰も入れない状態になった (#295)。

## Decision

**パッケージメタデータは変更せず、実行時にホストを見て判断する。**

### なぜ Recommends に昇格しないのか

Ubuntu 26.04 で `gnome-shell-extension-appindicator` は**実体のない仮想パッケージ**に
なっており、唯一の provider は `gnome-shell-ubuntu-extensions`、そしてそれ自身が
`Depends: gnome-shell (>= 49~)` である。apt は Recommends を既定で自動導入するため、
昇格すると **`apt install waired-tray` した server に GNOME Shell + gjs + gir 一式が
入る**。Depends はさらに悪い。

### なぜ「GNOME があるときだけ」を apt で書けないのか

Debian の依存フィールドに条件付きの形式は存在しない。`Depends` / `Recommends` は
無条件、`Suggests` は絶対に入らない、`A | B` は「どれかが入っていれば充足、無ければ
**先頭を入れる**」であって「既に入っている場合だけ」ではない。postinst からの apt は
dpkg がロックを保持しているため不可（既存コメントのとおり）。`waired-tray-gnome` への
パッケージ分割は可能だが、「それを入れると誰が決めるか」が結局実行時判定に戻るだけで、
公開パッケージが 1 つ増える。

### 実行時の主体は 3 つ。共通ルールは「gnome-shell が既に在るときしか install しない」

| 主体 | いつ | 権限 | 役割 |
|---|---|---|---|
| `install.sh` | 導入時 | 既に root | dpkg に gnome-shell があるときだけ拡張を同じ apt トランザクションに追加し、`SUDO_USER` に対して先出し enable |
| `waired-tray` | 毎セッション | 不要 | セッションバスを実測。拡張が在るのにホストが無ければ無権限で enable、拡張自体が無ければ 24h に 1 回トースト |
| `waired doctor --fix` | 手動 | sudo | apt まで含む受け皿（後から GNOME 導入 / tarball / 別ディストロ） |

daemon は不採用。root で動くが server でも動くので、層として不適切。

判定ロジックの本体は `internal/platform/trayhost.PlanRepair(goos, facts)` に純粋関数と
して置き、`GnomeShellOnPath` が false のとき決して install を計画しないことを
テーブルテストと全組み合わせの走査テストで固定した。install.sh は同じ規則を shell で
持つ（そちらは apt 経路専用なので `dpkg-query -W gnome-shell`、trayhost は tarball や
非 Debian でも答える必要があるので `$PATH`）。

### enable を「先に入れて終わり」にできない理由

enable の実体は per-user dconf の `org.gnome.shell enabled-extensions` 配列であり、
存在しない UUID を先に書いても無害かつ後から効く。しかしそれでは解決しない:

1. **per-machine ではない。** 導入時に手が届くのは `SUDO_USER` 1 人だけ。マシン全体に
   効かせるには dconf system profile が要るが、それは既定値なので、ユーザーが拡張機能
   アプリを一度触れば上書きされる。
2. **enable は何もインストールしない。** 拡張パッケージ自体が無いホストでは no-op。
3. **後から無効化され得る。** GNOME は `metadata.json` の `shell-version` を検証するので
   メジャーアップグレードで落ちる。ユーザーが切ることもある。

よって waired-tray の役割は「遅延 enable」ではなく**セッション開始ごとの実測と、タダで
直せる場合の修復**である。

## Consequences

- server への影響はゼロ。`gnome-shell` が無いホストでは apt トランザクションが従来と
  バイト単位で同一（`installtest-dash.sh` の否定ケースで全ホスト無条件に検証）。
- **Ubuntu Desktop では何も起きない**のが正しい。`ubuntu-desktop` →
  `gnome-shell-ubuntu-extensions` が `ubuntu-appindicators@ubuntu.com` を提供し、
  `/usr/share/gnome-shell/modes/ubuntu.json` の `enabledExtensions` で有効化されるため。
  実際に穴が空いているのは Debian GNOME / 非 Ubuntu GNOME / minimal GNOME と、
  「入っているが無効」のケース。
- 同じ判定が Go と shell の 2 か所にある。install.sh 側は apt 経路専用なので dpkg を
  見る、という差も含めて双方のコメントで相互参照している。
- アンインストール時に拡張は残す。ほかのトレイアプリが使っている可能性があり、
  apt が manually-installed として記録するので autoremove でも消えない。
- Windows / macOS は対象外（ネイティブのトレイホストを持つ）。`repair_stub.go` が
  API を揃え、`PlanRepair` は goos が linux 以外なら常に `RepairNone` を返す。

## Refs
- https://github.com/waired-ai/waired-agent/issues/295
- internal/platform/trayhost/repair.go — `PlanRepair` と server ガードの根拠
- packaging/install/install.sh — `linux_wants_tray_host_extension`
- packaging/nfpm/waired-tray.yaml.tmpl — `suggests` を維持する理由
