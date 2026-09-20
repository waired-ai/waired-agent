# OS のメモリ圧シグナルは、スラッシュのときしか鳴らない (20260920 19:00)

## Issue

#1453: メモリが足りなくなった時点で製品が読み込みを止め、人に小さい
モデルを勧める。オーナーの指示 (2026-09-20) は「単純なメモリ量ではなく、
OS のパラメータや例外として捕まえる方法を調べる」。

この記録は、Linux・macOS・Windows の 3 OS で実機のメモリ圧シグナルを
測った結果を残す。測定には使い捨ての割り当てツール (`memsqueeze`、
段階的に確保して触り、各段で OS のシグナルを採る) を使った。

## Learnings

### 1. 結論: どの OS でも、シグナルが出るのは回収/スワップが動いたときだけ

上限に当たって即死する形では、**直前まで何も鳴らない**。

| OS | 機体 | 条件 | 結果 |
|---|---|---|---|
| Linux | 120 GB 機 | cgroup 上限 4G、**スワップ無し** | PSI の `some`/`full` avg10 は上限まで **0.00 のまま**、そのまま **SIGKILL (137)**。予告は一切無い |
| Linux | 同 | cgroup 上限 4G、**スワップ 2G** | 上限でスワップアウトが始まり、PSI が **0.00 → 8.51 に約 1.3 秒で**上がる。スワップはまだ 1 GB 空いている段階 |
| macOS | 16 GiB / macOS 27.0 | 非圧縮データで確保 | 約 3.8 GiB でスワップ開始。`kern.memorystatus_level` が 53% → 31%。`kern.memorystatus_vm_pressure_level` が **6.1 GiB・2.3 秒の時点で 2 → 4 (CRITICAL)**。解放で即復帰 |
| Windows | 31.7 GiB / Win11 Pro | 同上 | 低メモリ通知が **空き 887 MB (memory load 97%) で発火**。高メモリ通知はその前、空き 1,769 MB で消える |

参照機 (127 GiB、Windows) の #1443 の落ち方は、空きが最後まで 28〜34 GB
あった (`docs/knowledges/20260920/0600-windows-igpu-memory-ceiling-below-fit.md`)。
**上の 3 つのシグナルは、どれもその状況では鳴らない。** メモリ圧の見張りで
#1443 を捕まえることはできない。閾値の調整で直る話ではない。

### 2. Windows の低メモリ通知は「小さい絶対値」で、大容量機では遅すぎる

`CreateMemoryResourceNotification` / `QueryMemoryResourceNotification` は
参照機でも動く (ハンドルは有効、静止時は low=未発火・high=発火)。
`LowMemoryThreshold` / `HighMemoryThreshold` はレジストリに置かれておらず、
既定値が効いている。31.7 GiB の機体で測った発火点は **空き 887 MB**。

- RAM の 2.8% であって、割合で決まっている風には見えない。同じ絶対値なら
  127 GiB の機体では 0.7% で、**ホストが完全にスラッシュしたあと**になる。
- したがって製品の Windows 側は、この通知だけでは大容量機を守れない。
  通知に加えて `GlobalMemoryStatusEx.AvailPhys` の割合の項が要る。
- レジストリの上書きはマシン全体に効くので、製品からは**読むだけ**にする。
- 参照機で同じ測定はしていない。空き 1 GB まで追い込む必要があり、それは
  #1443 で 2 回固まった状況そのものだから。設計の結論 (割合の項が要る) は
  1 点の測定で足りるので、危険を冒さない判断をした。

### 3. macOS のレベル 2 は「普通」で、止める条件にならない

静止時の `kern.memorystatus_vm_pressure_level` は、同じ機体で

- 空き 70% のとき **1 (NORMAL)**
- 空き 53% のとき **2 (WARN)**
- 実際のスラッシュ中 (空き 31%) に **4 (CRITICAL)**

だった。2 は半分空いている状態で出る早い助言で、異常ではない。**4 だけが
止める条件になる。**

### 4. macOS は匿名メモリを圧縮する — 合成テストは非圧縮データで書くこと

最初の計測は 4096 バイトごとに 1 バイト書く疎なパターンだった。これだと
**16 GiB の機体に 20 GiB を 2.3 秒で確保できてしまい、圧のレベルは
まったく動かない**。macOS のコンプレッサがほぼ無に潰すため。xorshift で
非圧縮のバイト列を書いたら、上の表のとおり素直にスワップと圧が出た。

モデルの重みは非圧縮なので、実際の読み込みでは圧が出る。落とし穴は
合成テストの側だけにある。

### 5. Linux の `oom_score_adj` は fork/exec をまたいで継承される

実測: 親に 753 を書いてから子を起こすと、子も 753。したがって、製品が
起こすエンジン 1 プロセスに書けば、その下の runner まで効く。cgroup の
委譲を用意する必要は無い。

### 6. cgroup に閉じ込めたスラッシュでも、ホストの操作感は失われる

上限 4G の cgroup を 2G のスワップごとスラッシュさせている間、ホストには
110 GB の空きがあったのに、ssh のセッションが 1 分以上返らなくなった。
#837 が報告している「箱ごと飢える」症状の縮小版。見張りは、シグナルが
最初に出た時点 (Linux なら約 1.3 秒) で動くべきで、レベルが育つのを
待つ設計にはしない。

### 7. 訂正: macOS の「空き」は減らない。ゲートに使えない

この記録の初版は、3 OS とも「空きが下限を割ったこと」を前提条件に使う
つもりで書いた。**macOS ではそれが成立しない。**

製品のコードを実機で走らせて分かった (20:00 の再計測):

| 確保量 | `kern.memorystatus_level` | スワップアウト | `vm_pressure_level` |
|---:|---:|---:|---:|
| 40,192 MB (16 GiB 機) | **36%** | 300〜500 MB/s | 2 |

16 GiB の機体に 40 GB を載せ、毎秒数百 MB をスワップに吐いている状態でも、
`kern.memorystatus_level` は 36% を保つ。下限 (RAM の 3%、最低 1 GiB) の
5 倍以上あり、**このゲートは永久に閉じない**。Linux の `MemAvailable` や
Windows の `AvailPhys` とは意味が違う (jetsam の会計であって、空き物理
メモリではない)。

したがって macOS だけ前提条件を変え、**「OS 自身が回収を始めたと言って
いる」(`vm_pressure_level` >= 2)** をゲートにした。静止時は 1 (空き 70%)
なので、ゲートが常時開きっぱなしにはならない。他の 2 OS より弱いゲート
なので、macOS ではスワップ速度の項が判断の多くを担うことになる。

直した後の実測: **10,752 MB の時点で critical**、スワップアウト
68.5 MB/s、26 秒。修正前は 40 GB まで行っても発火しなかった。

### 8. 製品コードの実機通し (3 OS、20:30 時点)

`internal/platform/mempressure` を実機で走らせた結果。誤検知チェックは
「平常運転のホストを 90 秒見て critical を 1 度も返さないこと」、検知
チェックは「本当にメモリを使い切ったときに critical を返すこと」。

| ホスト | 誤検知チェック | 検知チェック |
|---|---|---|
| Linux 124 GB | normal 90 / warn 0 / critical 0(最低空き 113,525 MB) | 107,520 MB 確保時に critical、977 MB/s、82 秒。**PSI 側で先に発火** |
| macOS 16 GB | normal 90 / warn 0 / critical 0(最低空き 11,468 MB) | 10,752 MB 確保時に critical、68.5 MB/s、26 秒 |
| Windows 31.7 GB (xps15) | normal 90 / warn 0 / critical 0(最低空き 5,050 MB) | Device Guard に拒否され未実施 (§9) |
| Windows 127 GiB (参照機) | normal 90 / warn 0 / critical 0(最低空き 61,236 MB) | 59,392 MB 確保時に critical、45 秒 |

3 OS とも、平常運転では 1 度も止めず、本当に使い切ったときは止める。
どのホストも自力で復旧し、固まった機体は無い。

経路が OS ごとに違うのが面白いところで、設計どおりでもある:

- Linux は **PSI**(予備経路)が、空きの下限を割る前に先に鳴った。
- macOS は **ゲート(レベル 2)+ スワップ急増**の組で鳴った。
- Windows (参照機) は **低メモリ通知**が鳴った。スワップアウトは 0.0 MB/s
  のままで、急増の項は一度も効いていない。

### 9. 補足: Windows の低メモリ通知は多少スケールする

参照機 (127 GiB) では、空き **2,766 MB では鳴らず、1,740 MB で鳴った**。
xps15 (31.7 GiB) の 887 MB と比べると、完全な固定値ではなく RAM に応じて
多少は動くらしい。ただしどちらも RAM の 3%(参照機で 3,905 MB)よりずっと
下なので、**大容量機では遅すぎる**という §2 の結論は変わらない。割合の項
を足した判断はそのままでよい。

### 10. Windows: xps15 では Device Guard でテストバイナリが走らない

sv-xps15 では `press.exe` (テストバイナリ) は 1 回目は走ったが、その後
「アプリケーション制御ポリシーによってこのファイルがブロックされました」
で実行できなくなった。この fleet は WDAC が効いている
(`internal/platform/appcontrol` が CodeIntegrity のログを読む機能を
持っているのは、まさにこのため)。

したがって Windows で取れた証拠は次の 3 つまで:

- 読み取り側は動く (misfire の実機チェックが 90 サンプル完走。PDH の
  カウンタと低メモリ通知の両方を読んでいる)。
- 判断は実測値を使ったテーブルテストで固定してある。
- 低メモリ通知の発火点 (空き 887 MB) はこのホストで測った。

**参照機 (sv-evox2) では同じバイナリがそのまま走った**ので、Windows の
通しはそちらで取れている (§8)。ポリシーはホストごとに違う。

### 9. 測るときの注意

- Linux の PSI は、自分の cgroup の `memory.pressure` も同じ書式で読める。
  共有ホストを squeeze するときは `systemd-run --user --scope -p MemoryMax=`
  に閉じ込めれば、ホストを巻き込まずに同じ曲線が採れる。
- 確保が速すぎると (メモリ帯域だけで 8 GB/s 出る) 0.5 秒刻みでは山を
  取りこぼす。段を小さくするか、保持時間を入れる。
- Windows の commit はどの回も上限に届かなかった (33.2 GB / 43.7 GB)。
  #1443 と同じで、commit はこの落ち方の律速ではない。

### 11. Windows の Job Object のコミット上限: 機構は効くが、採らない

#1453 の封じ込めの腕として、Windows の Job Object に
`JOB_OBJECT_LIMIT_JOB_MEMORY` を足す案を実機で確かめた(参照機、20:35)。
製品のサービスには触れず、別ポート・別ストアの ollama を Job Object の
中で起動して小さいモデル (`qwen3.5:0.8b-q8_0`、num_ctx 4096) を読ませた。

| 回 | 上限 | ジョブの commit の最大 | 結果 |
|---|---:|---:|---|
| 対照 | 無し | 2,085 MB | 読めた (2 秒) |
| 上限あり | 1,200 MB | **1,430 MB で頭打ち** | **落ちた** (4 秒) |

**機構は効く。** WDDM の確保はプロセスの commit に載るので、ジョブの上限で
本当に縛られる。上限の回は commit が 1,430 MB で止まり、対照の 2,085 MB に
届いていない。

**それでも採らない。** 落ち方が悪いからである。

- llama.cpp は確保の失敗を報告しない。`ggml.c:1643:
  GGML_ASSERT(ctx->mem_buffer != NULL) failed` で abort する。予想していた
  `ggml_vulkan: Memory allocation of size N failed.` は出ない。
- Windows はその abort を **exit status `0xc0000409`** として報告する。これは
  「スタックバッファのオーバーラン」と読める値で、実際にはそうではない。
  WER とログに、セキュリティ上の問題に見える記録が残る。
- そして上限には**数字が要る**。#1453 の出発点は「予算は推定で、当てにいく
  道は採らない」である。予算 (98,304 MB) を上限にしても、#1443 の落ちる
  読み込みの commit は約 87 GB なので**縛られない**。縛るには予算より下に
  置く必要があり、それは正常な読み込みを abort させる賭けになる。

**この計測から製品に入れたのは 1 つだけ**: `GGML_ASSERT(ctx->mem_buffer !=
NULL) failed` を、メモリ由来の読み込み失敗の署名として分類器に加えた。
ジョブの上限を入れなくても、本当にメモリが尽きたホストで同じ abort は
起こりうる。この署名が無いと、メモリを断られたエンジンが「壊れたエンジン」
と読まれ、同じ読み込みに再起動で戻ることになる。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1453
- https://github.com/waired-ai/waired-agent/issues/1443 / #1450 / #837
- `docs/knowledges/20260920/0600-windows-igpu-memory-ceiling-below-fit.md`
- https://docs.kernel.org/accounting/psi.html
- https://learn.microsoft.com/en-us/windows/win32/api/memoryapi/nf-memoryapi-creatememoryresourcenotification
- https://developer.apple.com/documentation/Dispatch/DISPATCH_SOURCE_TYPE_MEMORYPRESSURE
- https://man7.org/linux/man-pages/man5/proc_pid_oom_score.5.html
