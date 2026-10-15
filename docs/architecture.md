# 音声ゲートと Postman 接続境界

この文書は Issue #2 の小さな実装範囲を示します。現在のコードは iPhone または Mac のブラウザから Mac の Go ゲートウェイへ音声フレームを送り、Mac 側で発話を区切ってローカル `whisper-server` に日本語の文字起こしを依頼します。`whisper-server` とモデルは別プロセスで、起動・モデルの固定・実音声での精度確認は運用側の作業です。テストでは決定的な偽物を使い、実機や実モデルの成功を主張しません。

## 機器とプロセス

```mermaid
flowchart TB
  subgraph phone["iPhone または Mac のブラウザ"]
    mic["マイク / 一度の Start"] --> frames["20 ms PCM フレーム"]
    player["返信の再生 / Stop"]
  end
  subgraph mac["MacBook / Mac"]
    gateway["Go ゲートウェイ<br/>認証・音声上限・発話区切り"]
    whisper["別プロセス: whisper-server<br/>日本語 ASR"]
    adapter["テキストアダプター境界<br/>実接続は未設定"]
    tts["任意: Swift TTS ヘルパー<br/>現状は WAV 全体を保持"]
  end
  subgraph postman["Postman: 未接続の依存先"]
    inlet["#767 外部テキスト入口<br/>未実装・未検証"]
    pane["#901 単一の上位 agent pane<br/>人の通常の入力先"]
  end
  frames -->|"WebSocket / 上限あり"| gateway
  gateway -->|"メモリ内 WAV / HTTP"| whisper
  whisper -->|"成功時の最終テキストのみ"| gateway
  gateway --> adapter
  adapter -. "#767 完了後に接続" .-> inlet
  inlet -. "通常のユーザー入力" .-> pane
  pane -. "相関した終端結果" .-> inlet
  gateway -->|"任意の返信音声"| tts
  tts --> player
```

ブラウザは音声の境界を決めません。ゲートウェイは一つの前景セッションだけを受け付け、受信・発話・文字数・待ち時間を制限します。Go コードは入力音声をアプリ所有の永続ファイルに書きません。別プロセス、フレームワーク、OS の一時ファイルやスワップについて SSD 書き込みゼロは保証できません。

## リクエストと応答の順序

```mermaid
%%{init: {'sequence': {'mirrorActors': false}}}%%
sequenceDiagram
  participant B as ブラウザ
  participant G as Go ゲートウェイ
  participant W as 別プロセス ASR
  participant A as テキストアダプター
  participant P as Postman #767 / #901（未接続）
  B->>G: Start 後、世代付き PCM フレーム
  G->>G: Mac 側で発話を区切り、上限を確認
  G->>W: メモリ内 WAV / 日本語固定
  alt 無音・上限超過・ASR 失敗・タイムアウト
    G-->>B: エラーまたは次の待機状態
    Note over G,A: テキスト要求は 0 件
  else 最終テキストが確定
    W-->>G: 最終日本語テキスト
    G-->>B: 同じ turn / request ID の文字起こし
    G->>A: 固定テキストを一度だけ送信
    alt 現在の本番設定
      A-->>G: 未設定エラー（#767 待ち）
    else 決定的なテスト用偽物
      A-->>G: 相関した done / blocked または期限切れ
    end
    Note over A,P: #901 の pane 検証後、#767 の実入口へ接続予定
    G-->>B: エラーまたは任意の返信・音声
  end
```

Stage 1 は音声から一つの最終テキストを得るまでです。失敗した場合、アダプターへの呼び出しはありません。Stage 2 は固定済みのテキストから始まり、期限付きの一回の呼び出しで終端結果 `done` または `blocked` を受け取ります。現在の `UnavailableAdapter` は Postman へ何も送信しません。テスト用の偽物は二つのゲートと結合順序だけを確認します。実際の認証入口、永続的な重複排除、再試行・再起動時の相関、上位 pane への通常入力、端末結果の取得は Postman #901 と #767 の受入れ後に検証します。

アダプターが期限を無視して停止しない場合でも、ゲートウェイは期限で待機を終え、そのセッションを閉じます。遅れて返った結果は返信や TTS に使いません。アダプターの処理が実際に終了するまで次のセッションを受け付けないため、外部処理が残ったまま次の要求と重なることはありません。外部処理自体の取り消しや、その処理が行った副作用の取り消しは保証しません。

現行の TTS 経路は返信を WAV 全体としてバッファーに保持し、ブラウザへのイベントも全体を base64 で送ります。増分配信や物理 iPhone Safari の再生試験は完了していません。
