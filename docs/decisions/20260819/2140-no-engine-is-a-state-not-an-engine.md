---
status: accepted
---

# エンジンの不在は述語で答え、カタログはそれを文脈として述べる (20260819 21:40)

## Status
Accepted

## Context

pc-dell-premium (Windows, waired 0.0.3-rc3) の実機観測が発端 (waired-agent#852)。
同じホストの同じ起動で、CLI とデーモンログが正面から食い違っていた。

```
> waired runtimes ls
NAME       STATE       INSTALLED  MODE     CONTEXT  VERSION
ollama     not_started yes        spawned  -        -
```

```
INFO  engine decision  reason="no engine viable: vllm: no installed venv under the state dir;
                                ollama: no bundled binary installed"
WARN  ollama binary not found; inference subsystem will be inert until installed
      err="bundled ollama not installed (expected at C:\ProgramData\waired\runtimes\ollama\bin\ollama.exe)"
```

そのホストにエンジンは無い。issue の当初の見立て (「導入済みだが版が読めない」) は
この観測で否定された。独立した欠陥が 2 つあり、どちらも同じ形をしている ——
エンジンの有無を問うべき場所で問うていない。

### 1. `Installed` はリテラルの `true` だった

`runtimeStatusFor` (`cmd/waired-agent/inference.go`) は、レジストリが知っている
アダプタすべてに対して `Installed: true` を立てていた。アダプタは起動時に無条件で
登録されるので、「installed」という名前の唯一のフィールドが、何も問わない唯一の
フィールドだった。

`engine_resolve.go` は「この問いに答える場所は全部ひとつの解決を通れ」と宣言しており、
その理由として「手近な probe (`exec.LookPath`) はここでは誤りで、このリポジトリで
既に 4 回誤っている」と書いている。今回は 5 例目にあたるが、経路が違う ——
probe が悪いのではなく probe が無い。

この 1 フィールドを読む面が 3 つあり、いずれもエンジン不在のホストで誤っていた。

- `waired runtimes ls` の `INSTALLED` 列
- `waired status` の `if !r.Installed { continue }` —— 一度も発火したことのない枝
- `packaging/install/install.sh` の `waired_engine_installed`。その関数のコメントは
  既に意図を書いている (「明確な yes 以外は not-installed とし、『いつエンジンが入るか』を
  説明する側のバナー文を選ぶ」) が、デーモンが常に yes と答えるので完了バナーは常に
  「installed (local AI engine)」と主張していた。**シェル側の修正は不要** ——
  誤っていたのはデーモンの答え。

なお `VERSION` が空だったのは正しい。`entry.Version` はプロファイルの
`Engines.*.Installed` で守られており、そちらは正しく「無い」と答えていた。

### 2. カタログは在りもしないエンジンで全行を判定していた

`catalogEngine` (`internal/management/inference_catalog.go`) は「このホストが serve する
ことになるエンジン」を答える。未コミット時は auto-picker に落ち、venv が無ければ
vLLM を ollama に降格する。したがって**エンジンがゼロのホストでも ollama を名乗り**、
全 family がそれで判定される。実測ではそのホストに 14 行の通常の判定 (fits /
needs 24 GB / floored) が並び、**どの面にも「エンジンが無い」とは書かれていなかった**。
tray の Models サブメニューも同じ 14 行を出し、#842 以降その行は押せる。

これは #829 の 1 面隣にあたる。#841 は gateway のエンジン無しゲートを直したが、
カタログの判定面はその範囲外だった。

## Decision

1. **エンジンの有無は述語で答える。** `runtimeStatusFor` の `Installed` は
   `engineUsableOnHost` を通る。これは `hasUsableEngine` のエンジン別の腕を抽出した
   もので、両者が同じ規則を問うため `subsystem_state` と `INSTALLED` 列が同じホストに
   ついて食い違えない。解決順は従来どおり「エージェントのライブ resolver が先、
   TTL キャッシュされたプロファイルは resolver が配線されていないときだけ」。
   resolver の「no」は答えであって、プロファイルに聞き直す理由ではない。

2. **カタログはエンジンの不在を独立した事実として述べる。行の判定は消さない。**
   `ModelCatalogResponse.EngineInstalled` を追加する。判定を残すのは、それが
   「このコンピュータがエンジンを入れたら何を動かすか」についての真な言明であり、
   カタログが閲覧面でもあるため。欠けていたのは文脈であって判定ではない。

   `Engine` フィールドは従来どおり「このホストが使うことになるエンジン」を名乗り続ける。
   空にすると全行が「このエンジン向けの build が無い」に化けて判定が失われる。

3. **`EngineInstalled` はポインタである。** クライアントが「エンジンが無い」と
   「このデーモンはこのフィールドを知らない」を区別できなければならない。nil は
   unknown であって absent ではなく、そのとき各面はフィールド導入前とまったく同じに
   描画する。デーモンは runtime エントリからこの値を埋めるので、決定 1 の答えと
   同一であり第二の意見にならない。

4. **表示は必ず両方を言う。** 「このコンピュータに AI エンジンが無い」と
   「だから要求は他のコンピュータへ行く」。前者だけだと「このコンピュータは壊れている」と
   読めるが、そうではない —— エンジン不在は enroll 済みのまま mesh にルーティングする
   正常な状態である (waired-agent#387、#841。waired-ai/waired#1067 の決定 5 が
   「『エンジンなし』は正常状態」として記録している)。

5. **エンジン不在のホストでモデル行を押したら、そう告げてエンジンの導入を申し出る**
   (オーナー裁定 2026-08-19)。これはブロックではない。#842 は「モデルをグレーアウトする
   能力」を型ごと削除しており、それは維持する。**waired#1067 の warn-and-ask 裁定は
   容量ゲートについてのものであり、エンジンの有無は別の層である** (同裁定の表題・
   Context・引用されたオーナー発言のいずれも容量についてのもので、その決定 5 は
   むしろエンジン不在を正常状態として認めている)。エンジンが無いホストでモデルを
   選ぶ操作自体には意味がある —— 「エンジンを入れたら動かすモデル」の予約であり、
   塞ぐ理由がない。

6. **導入を先、記録を後。** preference は導入が成功したときにだけ書く。先に書くと、
   切り替える対象を持たないデーモンに切り替えを投げることになり、Windows では
   再起動フォールバックに落ちてサービスが戻らない (waired-agent#855)。#855 自体は
   この変更では直らず、別件として残る。

7. **セットアップウィザードのモデル一覧では導入を再提案しない。** 7 ステップの導入
   フロー (waired 側 20260808/2325 の決定 4) はエンジン導入の可否をこれより前の
   ステップで尋ねている。エンジンが無い状態でこの一覧に到達したことは既に下された
   選択であり、同じ問いを重ねることになる。文脈を 1 文添えるにとどめる。

## この決定に含まれないもの

- **なぜそのホストにエンジンが無かったのか。** この記録は報告の誤りを直すもので、
  導入経路の欠落があればそれは別に追う。要求時のみ導入する設計なら、この issue は
  純粋に報告の話になる。
- **waired-agent#855** (Windows で再起動フォールバック後に SCM がサービスを戻さない)。
  決定 6 は「エンジン不在から #855 に落ちる」経路を消すが、#855 は開いたまま。
- **エンジンは在るが pin 未満の場合の文言。** それは #853 で着地済み
  (docs/decisions/20260819/1910-an-engine-floor-degrades-with-a-reason.md)。

## Refs
- https://github.com/waired-ai/waired-agent/issues/852
- https://github.com/waired-ai/waired-agent/issues/842
- https://github.com/waired-ai/waired-agent/issues/855
- https://github.com/waired-ai/waired-agent/issues/841
- https://github.com/waired-ai/waired-agent/issues/829
- https://github.com/waired-ai/waired-agent/issues/387
- https://github.com/waired-ai/waired/issues/1067
- docs/decisions/20260819/1910-an-engine-floor-degrades-with-a-reason.md
