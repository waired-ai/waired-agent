# ROCm は Strix Halo の Windows でも動く — それでも Vulkan が速く、両方在ると ollama が自分で選ぶ (20260906 04:30)

## Issue

waired-agent#1233 の実測記録。同 issue は「Windows の ROCm allowlist
(`amdROCmSupportedRes`、internal/runtime/ollama_backend.go) は 2 版前の
オーバーレイに対して押された刻印のままで、Strix Halo は Vulkan 専用では
なくなっているかもしれない」と言い、regex の編集ではなく計測を求めた。
これがその計測。waired-ai/waired#1312 のレーン L100 の一部。

読者は、次に `ResolveOllamaBackend` の GPU バックエンド判断を変える人と、
1 台の機械で ollama のバックエンド 2 つを A/B しようとする人。先に §4 を
読むこと — 最初の実験は 2 時間分まるごと無効で、しかも結果は正常に見えた。

計測環境 (2026-09-06): sv-evox2 — Windows 11 26200 / AMD Ryzen AI Max+ 395
(Strix Halo、Radeon 8060S) / ユニファイドメモリ 127.15 GB / ollama 0.33.3
の base に ROCm オーバーレイを手で `C:\l100` へ展開。モデルは
`qwen3.6:35b-a3b-q4_K_M` (21.80 GB、出荷済みカタログタグ)。

## Learnings

### 1. 「ROCm has no Windows APU support」は 0.33.3 では偽

`ResolveOllamaBackend` の Strix Halo on Windows の腕は、コメントとユーザーに
見える `Reason` の両方でこう言っていた: "ROCm has no Windows APU support;
Vulkan is the only GPU path"。ollama 0.33.3 ではそうではない。

- Windows の ROCm オーバーレイは `lib/ollama/rocm_v7_1` (ROCm 7.1。コメントの
  刻印は v6.1 だった)。
- その rocBLAS カーネルは gfx906, gfx1030, gfx1100, gfx1101, gfx1102,
  gfx1150, gfx1151, gfx1200, gfx1201 を持つ。zip の中身だけでなく、この
  機械のディスク上で確認した。
- オーバーレイが在ると、エンジンはこう報告する:

  ```
  library=ROCm compute=gfx1151 name=ROCm0 description="AMD Radeon(TM) 8060S Graphics" libdirs=ollama,rocm_v7_1 pci_id=0000:c5:00.0 type=iGPU total="76.8 GiB" available="76.6 GiB"
  ```

- Vulkan を退避した状態では、21.80 GB のモデルを丸ごと GPU で配信した:
  `/api/ps` の `size_vram` は `size` と等しく、dispatch 行は
  `[{ID:0 Library:ROCm}]`、`load_tensors: ROCm_Host ...`。

つまり ROCm は Windows の Strix Halo で**動く**。動かないから Vulkan、という
理由は消えた。

### 2. それでも Vulkan が正しい — 数字で

温かいターン、約 9,910 トークンのプロンプト、runner のフラグは全構成で同一
(`-c 32768 -np 1 -b 1024 -ub 1024`。エンジン自身のバッチサイジングが混入
しないようにした):

| 構成 | 応じたデバイス | prefill tok/s | decode tok/s | 露出 VRAM |
|---|---|---|---|---|
| Vulkan 単独 (rocm を退避) | `[{ID:0 Library:Vulkan}]` | 900.2 | 57.40 | 102.2 GiB |
| Vulkan (両方在中・独立再起動 3 回) | 同上 | 907.0 / 905.5 / 905.1 | 57.82 / 57.56 / 57.98 | 102.2 GiB |
| ROCm 単独 (vulkan を退避) | `[{ID:0 Library:ROCm}]` | 818.9 | 51.77 | 76.8 GiB |
| ROCm 単独 + `HSA_OVERRIDE_GFX_VERSION=11.5.1` | 同上 | 857.9 | 51.50 | 76.8 GiB |

Vulkan は ROCm の最良構成に対して prefill で約 4.9 %、decode で約 11.5 %
速い。HSA override は ROCm の prefill をいくらか買い、decode は買わない。

露出 VRAM の列は速度と同じだけ重い。ROCm はユニファイドプールのうち
76.8 GiB を見、Vulkan は 102.2 GiB を見る。つまり同じ機械で、Vulkan なら
載るモデルが ROCm では載らないことがある。
docs/knowledges/20260906/0400-an-ollama-tag-needs-a-renderer.md §4 の 79 GB
モデルは常駐 51.3 GiB を要した — ROCm の下では余裕がはるかに薄かった。

**測っていないこと**: 深いコンテキストでこの順位が保たれるか。ただし
未検証のリスクの向きも Vulkan に有利で、KV に使える余地が大きいのは
Vulkan のほう。

### 3. 両方がディスクに在ると ollama が選ぶ — そして Vulkan を選ぶ

修正の形そのものを変える所見。独立した再起動 4 回 — `OLLAMA_VULKAN=1`
有り、無し、`HSA_OVERRIDE_GFX_VERSION=11.5.1` 有り、素 — のすべてで、
エンジンは**両方**のデバイスを発見し (`total_vram="179.0 GiB"`、
102.2 + 76.8)、すべてで `[{ID:0 Library:Vulkan}]` に dispatch した。

したがって Windows の腕に ROCm のステップを足しても、どのバックエンドが
配信するかは変わらない。変わるのは、インストーラが約 250 MB のオーバーレイ
を落とし、そのあと誰も使わないことだけ — `wantROCmOverlay`
(cmd/waired/runtimes_install_windows.go) が訊くのは
`BackendPlan.WantsROCm` だから。それでも ROCm を使いたい人は
`WAIRED_OLLAMA_GPU_MODE=rocm` を設定する (同関数が先に読む)。

### 4. 罠 — 環境変数は ollama のバックエンドを切り替えない

最初の実験は無効で、しかも正常に見えた。3 モード — `OLLAMA_VULKAN=1`、
`OLLAMA_VULKAN` 無し、`HSA_OVERRIDE_GFX_VERSION=11.5.1` — を回して、
prefill 905 / 905 / 903、decode 57.8 / 57.6 / 58.0 を得た。ほぼ同一の数字は
「ここではバックエンドは効かない」と読める。同一だったのは、**3 本とも
Vulkan で走っていた**から。オーバーレイがディスクに在るので、エンジンは
毎回 ROCm を発見し、毎回 Vulkan を選び、上の変数のどれもそれを覆さなかった。

見分けるのは速度ではない。dispatch 行
`runner.inference="[{ID:0 Library:Vulkan}]"` と、バッファ行
`load_tensors: Vulkan0 model buffer size = ...` である。**2 本の run を比べる
前に、どのデバイスが実際に応じたかを読むこと。**

修正の中にもう 1 つ罠がある。バックエンドのディレクトリをその場で改名しても
**隠れない** — `lib/ollama/vulkan` を `vulkan.off` にしても拾われ、ログは
`libdirs=ollama,vulkan.off` と読めた。ollama は `lib/ollama` の
サブディレクトリを名前にかかわらず列挙する。ディレクトリはそのツリーの
**外へ移動**しないといけない。

### 5. 開いたまま残るもの

#1233 の RDNA4 の半分は決着していない: gfx1200/1201 はオーバーレイに
載っているが、`amdROCmSupportedRes` に RX 9000 のパターンは無く、測れる
カードがフリートに 1 枚も無い。安全側に外れる — そのホストは Vulkan へ行き、
Vulkan は動く。

もう 1 点。この allowlist が決めるのはオーバーレイを**取ってくるか**だけで、
両方が在ればエンジンが自分で選ぶ (§3)。つまり allowlist はバックエンドの
判断というより、ダウンロードの判断である。

## 補足 (2026-09-06、同日): いつから動いていたのか

上の §1 は「0.33.3 では偽」と見出しを立て、0.33.3 でそれが変わったかのように
読める。**この「いつ」は計測ではなく、コメントの刻印 (「Ollama 0.31.x, ROCm
v6.1 overlay」) からの推論だった。** 同日に 2 つ確かめて、答えは違った。

1. Windows の ROCm オーバーレイの資産を 5 リリース分 — v0.31.1, v0.32.13,
   v0.32.15, v0.33.2, v0.33.3 — について読んだ (zip の末尾を ranged GET で
   取り、rocBLAS カーネルを列挙)。**5 本すべてが `rocm_v7_1` に展開され、
   5 本すべてが同一のターゲット集合を持つ**: gfx906, gfx906-xnack-, gfx1030,
   gfx1100, gfx1101, gfx1102, gfx1150, gfx1151, gfx1200, gfx1201。つまり
   変わったのはオーバーレイの中身ではない。コメントの刻印は、自分が名指し
   した版についてさえ誤っていた — v0.31.1 の時点で v7.1 だった。

2. カーネルが同梱されることと ROCm が応じることは同じではないので、そちらも
   測った。ollama **v0.31.1** と **v0.33.2** をそれぞれのオーバーレイと共に
   同じ機械 (sv-evox2) に展開し、`OLLAMA_IGPU_ENABLE=1` で起動して、起動時の
   discovery 行を読んだ。モデルは要らない。両方とも、0.33.3 と字面まで同じ
   行を出す:

   ```
   library=ROCm compute=gfx1151 name=ROCm0 description="AMD Radeon(TM) 8060S Graphics" libdirs=ollama,rocm_v7_1 pci_id=0000:c5:00.0 type=iGPU total="76.8 GiB" available="76.6 GiB"
   ```

   そして両方とも、その隣に Vulkan も見ている (`total_vram="179.0 GiB"`)。

正直な言い方はこうなる: **「ROCm has no Windows APU support」は 0.33.3 で
偽になったのではなく、この製品が pin してきたどのエンジンでも偽だった** —
少なくとも、刻印自身が名指しした v0.31.1 まで遡って。§2 (Vulkan が速い)、
§3 (ollama が自分で選ぶ)、§4 (罠)、§5 (RDNA4) は何も変わらない。変わるのは
「いつ」だけ。

再利用できるのは次の 1 点。この訂正の最初の版も推論だった — 「オーバーレイに
gfx1151 が入った」から「0.33.3 で変わった」を導いていた。§4 の教訓を 1 段
上に当てただけのこと: コメントの版刻印は、その版についての証拠ではない。
証拠は資産と、動いているエンジンにある。どちらも後から確かめるのは安かった
(zip の central directory への ranged HTTP request 1 本と、モデル無しの
14 秒のサーバ起動) ので、先に確かめない理由は無かった。

同じ PR で internal/runtime/ollama_backend.go のコメントも同じことを言うよう
直してある。記録とツリーが一致する。

## 補足 2 (2026-09-06、同日): §2 の測り方が不十分だった

§2 は prefill 900 対 858、decode 57.4 対 51.5 を並べ、Vulkan が約 4.9 % と
約 11.5 % 速いと結論した。**どの数字も、バックエンドごとに 1 ターン、
num_predict 64 で採ったもの** — decode の窓は約 1.2 秒。同じ Vulkan 構成が
あるセッションでは decode 57.4、別のセッションでは 44.88 を出しており、
その振れ幅 22 % は主張している差より大きい。**この程度の差は、片方
1 ターンずつでは読めない。**

測り直しを強いたのはオーナーの 2 つの指摘だった。ROCm についての結論が
それまでの検証履歴と食い違うこと、そして露出メモリの 102.2 GiB 対
76.8 GiB という差が正しく見えないこと。

### まず、ROCm は本当に GPU に載っているのか (§1 の証拠も不完全だった)

§1 が「ROCm が応じた」と結論した根拠は、ollama 自身の dispatch ラベル
`[{ID:0 Library:ROCm}]` と、`/api/ps` の `size_vram == size` である。
どちらも llama.cpp 自身の勘定ではない。§1 が引いた
`load_tensors: ROCm_Host model buffer size = 272.81 MiB` は**ホスト**側の
バッファで、デバイス側の行は読んでいなかった。

測り直した。対照として、`lib/ollama` から**両方**のバックエンドを外へ移動した
CPU 単独も採った:

```
ROCm      load_tensors:  ROCm0  model buffer size = 21171.18 MiB   (device)
          llama_kv_cache: ROCm0 KV buffer size = 640.00 MiB
          using device ROCm0 (AMD Radeon(TM) 8060S Graphics) (0000:c5:00.0) - 78197 MiB free
          GPU 使用率のピーク 453.2 %
Vulkan    load_tensors: Vulkan0 model buffer size = 21171.18 MiB   (device)
          using device Vulkan0 (AMD Radeon(TM) 8060S Graphics) - 99437 MiB free
          GPU 使用率のピーク 112.2 %
CPU 単独  load_tensors: CPU model buffer size = 9196.26 MiB + CPU_REPACK 12247.73 MiB
          size_vram = 0.00 GB、GPU 使用率 0.0 %、prefill 158.1、decode 19.33
```

§1 の結論は保つ。ただし今の根拠はラベルではなく、デバイスバッファと対照で
ある: 両方とも本当に GPU で走っており、CPU は遅いほうのバックエンドに対して
さえ prefill で 4.0 倍、decode で 2.3 倍遅い。

### 次に、雑音が潰れる大きさで測り直す

オーナーの提案は「計測を大きくする」だった。プロンプトは 30,011〜36,010
トークン (前は約 9,910)、num_predict 512 (前は 64。decode の窓は約 1.2 秒
から約 10 秒へ)、num_ctx 65536。各ラウンドの中でバックエンドを交互に回し
(ドリフトが片方に味方できないように)、2 ラウンド。ロード直後の冷たい
ターンは捨て、ラウンドごとバックエンドごとに 3 ターンを採る — 各 6 サンプル。

| | 中央値 | 最小 | 最大 | 幅 |
|---|---|---|---|---|
| prefill ROCm | 636.3 | 580.0 | 654.1 | 11.6 % |
| prefill Vulkan | **876.8** | 839.4 | 912.6 | 8.4 % |
| decode ROCm | 43.8 | 43.1 | 44.3 | **2.7 %** |
| decode Vulkan | **49.2** | 47.9 | 50.1 | 4.4 % |

**どちらの軸でも範囲が重ならない**: ROCm の最良 prefill 654.1 は Vulkan の
最悪 839.4 を下回り、ROCm の最良 decode 44.3 は Vulkan の最悪 47.9 を下回る。
中央値で Vulkan が prefill 37.8 %、decode 12.3 % 速い。decode の幅が 2.7 %
まで落ちたことに注意 — 窓を伸ばした効果はそれがすべてで、差がそもそも
読めるようになった理由もそれである。

つまり §2 の**結論**は生き残り、**数字**は生き残らない。Vulkan は速い —
prefill では §2 の主張より広い差で、decode ではほぼ同じ差で。§2 の数字は
引用しないこと。

### 再利用できるもの

2 点、そのまま書く。

1. **小さな差を信じる前に、計測の窓の大きさを決める。** 1.2 秒の decode 窓が
   測るのはスケジューリングとウォームアップであって、スループットではない。
   中央値と並べて最小・最大を書き、差が幅を越えて残るかを読者が見られる
   ようにする。範囲が重なるなら、報告すべき差は無い。
2. **構成は 1 回の run の中で交互に回す。** 最初の計測はバックエンドを
   それぞれ自分のセッションで 1 回ずつ測ったので、セッション間のドリフトと
   バックエンドの差が区別できなかった — 実際、あるセッションでは ROCm が
   前に出て、別のセッションでは Vulkan が前に出た。

### やっている途中で踏んだハーネスの罠 (2 時間分なので行を割く)

- **PowerShell の変数名は大文字小文字を区別しない。**
  `foreach ($m in 'rocm','vulkan')` がモデルタグの `$M` を黙って上書きし、
  すべてのリクエストが `rocm` という名のモデルを求めて HTTP 404 を受けた。
  この 404 を起動時の競合と診断したのは誤りだった — そのために足した
  readiness 確認は改善ではあったが、原因ではなかった。ループ変数は大文字
  小文字で衝突し得ない名前にする。
- **BOM の無い .ps1 は Windows PowerShell 5.1 に ANSI コードページとして
  読まれる。** フォーマット文字列の中の日本語が文字化けして文字列を閉じ、
  パーサエラーになった。リモートへ送るスクリプトは ASCII だけにし、走らせる
  前にその機械で構文検査する
  (`[System.Management.Automation.Language.Parser]::ParseFile`)。
- **`/api/version` はモデル一覧が揃う前に応える。** タグが `/api/tags` に
  現れるまで待つ。

同じ PR で internal/runtime/ollama_backend.go の Strix Halo の腕の数字を
これらに置き換え、測り方を隣に書いてある。記録とツリーが一致する。

## この PR で変えたもの

挙動は変えていない。コードと本記録が一致するよう:

- Strix Halo on Windows の腕のコメントと `Reason` は、ROCm の不在を主張する
  代わりに "measured faster than ROCm here" と言う。
- `amdROCmSupportedRes` の上の `!!! MAINTENANCE` ブロックは、gfx1151 の半分が
  決着し、RDNA4 の半分が未決であることを記録する。
- 同じ腕のコメントの数字は補足 2 の測り直しのもの (中央値と範囲、測り方
  つき)。§2 の数字はコードに残っていない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1233
- https://github.com/waired-ai/waired-agent/issues/1193
- https://github.com/waired-ai/waired-agent/issues/1192
- https://github.com/waired-ai/waired/issues/1312
- docs/knowledges/20260906/0400-an-ollama-tag-needs-a-renderer.md
- docs/decisions/20260829/1600-move-both-engine-pins.md
