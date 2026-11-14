# uo-detection-bot

Raspberry Pi (Linux ARM64) で動かす、小さな Go 製 Discord Bot です。
現時点の機能は `/ping` のみで、`🏓 Pong! 42 ms` のように Discord Gateway の遅延を返します。
文字・音声・画像の検知機能はまだありません。

元のソースが残っていなかったため、既存実行ファイルのビルド情報、関数名、コマンド説明、応答書式を調べて新しく実装しました。元ソースの完全な復元ではありません。

## 開発

`go.mod` の Go バージョンを使用します。

```sh
go test -race ./...
go vet ./...
python3 -m unittest discover -s scripts -p 'test_*.py' -v
CGO_ENABLED=1 go build -tags netcgo -o /tmp/discord-bot ./cmd/discord-bot
```

実行時は `DISCORD_TOKEN` を環境変数として指定します。`.env` の自動読み込みは行いません。
`DISCORD_GUILD_ID` を設定するとそのサーバーに `/ping` を登録し、空ならグローバル登録します。
指定サーバーには Bot が参加している必要があります。権限エラー時に別のサーバーやグローバル登録へ自動で切り替えることはありません。
メッセージ本文へのアクセスや特権 Intent は不要です。他のアプリケーションコマンドは削除しません。

## Raspberry Piへの初回設置

Debian系 ARM64、glibc 2.39以上、systemd、Python 3 が必要です（Debian 13で動作確認）。
配布物はGitHubのUbuntu 24.04 ARM64環境でビルドし、`netcgo`でOS標準の名前解決を使います。
このPiではGo単独のDNS解決が失敗し、OS標準の名前解決は成功するためです。
Discord Developer Portal で Bot を作成し、`bot` と `applications.commands` スコープでサーバーに追加してください。

```sh
sudo install -o root -g root -m 0600 .env.example /etc/discord-bot.env
sudoedit /etc/discord-bot.env
sudo bash scripts/install.sh
```

上の設定ファイル作成は初回のみです。既存の `/etc/discord-bot.env` を上書きしないでください。
トークンは Pi 内だけに置き、GitHub Secrets、コード、Issue、ログには書きません。
Bot はログインできない専用ユーザー `discord-bot` で動作します。更新処理と実行ファイルは root が所有します。
インストーラーは既存のサービス設定・実行ファイルを `/var/backups/discord-bot.*` に保存します。
最初の成功済み Release がまだなければ、更新タイマーが完成を待ちます。

## main更新から自動反映まで

1. `main` への push で GitHub ホストの Actions が Go/Python のテストと `go vet` を実行。
2. 成功時に Linux ARM64 バイナリと `SHA256SUMS` を作成。
3. `deploy-<40桁コミットSHA>` の Release を、両ファイルのアップロード完了後に公開。
4. Pi の systemd timer が前回処理終了から5分後（最大15秒のゆらぎあり）に確認。
5. 現在の `main` と一致する完成済み Release を取得し、チェックサムとARM64形式を検証。
6. `current` シンボリックリンクを切り替え、Botを再起動。
7. Discord接続と `/ping` 登録が完了した起動通知を待ち、さらに10秒間プロセスが安定していれば成功。

ルーターのポート開放、Pi上のGitHubランナー、PiへのGitHub書き込み資格情報は不要です。
更新時には短時間の停止があります。CI失敗、未完成Release、通信失敗では旧版を維持します。
起動失敗・直後のクラッシュは旧版に戻し、失敗したSHAの自動再試行を停止します。
最初の設置で起動失敗した場合は旧リリースがないため停止します。元の実行ファイルと設定のバックアップは残ります。
更新処理が途中で中断された場合も、次回実行時に記録された旧版へ戻します。
Gatewayの応答停止はsystemdのwatchdogで検出・再起動します。起動後の全ての機能不具合を自動検知するものではありません。

自動配布するのは **Bot実行ファイルだけ** です。`deploy/` と更新スクリプトの変更は、Piで内容を確認してインストーラーを再実行してください。
公開済みReleaseの同じタグを上書きせず、修正は新しいコミットとして配布します。古いリリースは復旧のため保持するので、ディスク容量に応じて整理してください。

## 運用

```sh
systemctl status discord-bot.service discord-bot-update.timer
journalctl -u discord-bot.service -n 50
journalctl -u discord-bot-update.service -n 50
sudo systemctl start discord-bot-update.service
```

不具合時は `main` の問題の変更を revert し、新しいコミットを push すると、検証済みの修正版が自動配布されます。
一時的な認証エラーなどを修正して同じSHAを再試行したい場合は、更新タイマーと更新サービスを停止したうえで `/var/lib/discord-bot-update/state.json` の `failed` フィールドだけを取り除き、タイマーを再開してください。`pending` は中断処理の復旧に必要なので削除しないでください。
トークンを更新した場合は `sudo systemctl restart discord-bot.service` を実行します。

GitHubの `main` は、PRと `verify` の成功を必須にするブランチ保護を推奨します。
