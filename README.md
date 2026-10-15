# tmux-mac-voice

機器・プロセスの配置と Stage 1 / Stage 2 の順序は [音声ゲートと Postman 接続境界](docs/architecture.md) を参照してください。

iPhone Safari の一度の Start から Mac に音声を継続送信し、Mac 側で発話を区切って日本語の文字起こしを行う試作です。Stop はセッション全体を終了します。Postman の外部テキストアダプターは [#767](https://github.com/i9wa4/tmux-a2a-postman/issues/767) で別途作業中で、この版からエージェントへの実配送はまだできません。

## ローカル試験

macOS、Go、Swift コンパイラー、ローカルの `whisper-server`、日本語対応の多言語モデルが必要です。`whisper-server` は `127.0.0.1` で起動し、`/inference` に WAV を受け付ける設定にします。音声の一時ファイルを避けるため、サーバーの変換オプション（`--convert`）は有効にしないでください。モデルやサーバーはこのリポジトリに含まれません。

```sh
swiftc tools/tts.swift -o /tmp/tmux-mac-voice-tts
export VOICE_TOKEN="$(openssl rand -hex 24)"
export VOICE_ORIGIN=http://127.0.0.1:8765
export VOICE_WHISPER_URL=http://127.0.0.1:8080/inference
export VOICE_TARGET=agent
export VOICE_TTS_HELPER=/tmp/tmux-mac-voice-tts
export VOICE_DEMO=1
go run .
```

Mac のブラウザで `http://127.0.0.1:8765` を開き、`VOICE_TOKEN` の値を接続キー欄に入れます。`VOICE_DEMO=1` は認識したテキストをそのまま読み上げる**ローカル試験専用**の返信です。Postman への配送は行いません。iPhone から試すには、Tailscale Serve でローカルゲートウェイだけを HTTPS に中継し、`VOICE_ORIGIN` をブラウザに表示される HTTPS オリジンと完全一致させます。

Go とブラウザコードの再現可能な検証には、`flake.lock` で固定した Nix ツールチェーンを使います。macOS では次を実行します。

```sh
nix flake check --print-build-logs
nix develop --command sh -c 'go version && node --version'
nix develop --command sh -c 'test -z "$(gofmt -l .)"'
nix develop --command sh -c 'go test ./... && go test -race ./...'
nix develop --command sh -c 'node --check web/app.js && node --check web/worklet.js && node --test app_test.cjs'
swiftc tools/tts.swift -o "$TMPDIR/tmux-mac-voice-tts"
```

`nix flake check` には Go の通常・race テストとブラウザコードの構文・単体テストを含めます。Swift ヘルパーのコンパイルは macOS の Xcode ツールチェーンで行い、GitHub Actions でも同じ構成を使います。`go.mod` の `go 1.23` は言語バージョンの下限で、実行する Go のパッチ版は `flake.lock` の nixpkgs に従います。Swift の音声生成は AVSpeechSynthesizer のメモリ内コールバックを使います。`say -o /dev/stdout` はこの Mac で出力できなかったため、`say` コマンドを直接使う構成にはしていません。

## 境界と未確認事項

- Safari は 20 ms の 16 kHz PCM フレームを一本の WebSocket で送ります。Mac の単純な音量閾値と 700 ms の無音でターンを区切り、20 秒を超える発話は破棄します。雑音や小声での精度、Safari 実機での継続・再生復帰は未検証です。
- Go は音声をファイルに保存しません。Whisper へはメモリ上で作った WAV を送ります。外部 ASR/TTS や OS の一時ファイル・スワップまで SSD 書き込みゼロとは保証しません。
- ブラウザと Go は録音と再生を半二重にし、世代と結果 ID で古いフレームや再生終了通知を拒否します。再生中の音声の再収録防止は実機で検証が必要です。
- `VOICE_DEMO` を外すと、テキストアダプターは明示的に「未設定」を返します。実運用には [#901](https://github.com/i9wa4/tmux-a2a-postman/issues/901) の実ペイン接続と [#767](https://github.com/i9wa4/tmux-a2a-postman/issues/767) の認証済みテキスト入口が必要です。再起動後も同じ依頼 ID の再送を一度だけ処理する永続的な重複排除、最終返信の相関、タイムアウト処理との接続と再起動試験は未実装・未検証です。
- 接続キー、正確な Origin、ローカルの待ち受け、一つの同時セッションで入口を制限します。Tailscale 接続だけを本人確認として扱いません。
