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

### 7. 測るときの注意

- Linux の PSI は、自分の cgroup の `memory.pressure` も同じ書式で読める。
  共有ホストを squeeze するときは `systemd-run --user --scope -p MemoryMax=`
  に閉じ込めれば、ホストを巻き込まずに同じ曲線が採れる。
- 確保が速すぎると (メモリ帯域だけで 8 GB/s 出る) 0.5 秒刻みでは山を
  取りこぼす。段を小さくするか、保持時間を入れる。
- Windows の commit はどの回も上限に届かなかった (33.2 GB / 43.7 GB)。
  #1443 と同じで、commit はこの落ち方の律速ではない。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1453
- https://github.com/waired-ai/waired-agent/issues/1443 / #1450 / #837
- `docs/knowledges/20260920/0600-windows-igpu-memory-ceiling-below-fit.md`
- https://docs.kernel.org/accounting/psi.html
- https://learn.microsoft.com/en-us/windows/win32/api/memoryapi/nf-memoryapi-creatememoryresourcenotification
- https://developer.apple.com/documentation/Dispatch/DISPATCH_SOURCE_TYPE_MEMORYPRESSURE
- https://man7.org/linux/man-pages/man5/proc_pid_oom_score.5.html
