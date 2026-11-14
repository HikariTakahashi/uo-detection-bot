# uo-detection-bot

Raspberry Pi (Linux ARM64) で動かす、小さな Go 製 Discord Bot です。
`/ping` で `🏓 Pong! 42 ms` のように Discord Gateway の遅延を返します。
指定チャンネルで冷笑ワードと完全一致する新規投稿を検知し、その投稿への返信で投稿者へのメンションと累計回数を通知します。設定で「対象ワード＋w」のAI判定も有効にできます。
設定すると、毎朝のYouTube通知と毎週金曜のメンションも送信します。
音声・画像の検知機能はありません。

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
検知が無効な場合、メッセージ本文へのアクセスや特権 Intent は不要です。他のアプリケーションコマンドは削除しません。

## 冷笑ワードの検知

設定ファイルは既定で `/etc/discord-bot/config.json` です。`detection.config.example.json` を参考に設定します。
初期ワードは「うお」「どわー」、初期チャンネル一覧は空です。ファイルがない場合や `channel_ids` が空の場合は検知を無効にします。
ファイルがある場合は `words`（空でない文字列の配列、1語以上）と `channel_ids`（チャンネルIDの文字列配列）が必須です。不正なJSON・設定値は起動エラーになります。

```json
{
  "words": ["うお", "どわー"],
  "channel_ids": ["123456789012345678", "234567890123456789"]
}
```

上記IDは例です。Discordの開発者モードを有効にし、対象チャンネルの「チャンネルIDをコピー」で取得した値に置き換えてください。
指定IDだけを対象にし、配下のスレッドは自動では含めません。スレッドも検知する場合はそのIDを個別に追加します。
`DISCORD_GUILD_ID` は `/ping` の登録先だけに影響し、検知対象は `channel_ids` で決まります。

検知を有効にする前に、Discord Developer Portal の Bot → Privileged Gateway Intents で **Message Content Intent** を有効にしてください。
Botには対象チャンネルの「チャンネルを見る」「メッセージを送信」「メッセージ履歴を読む」を許可します。スレッドでは参加・閲覧可能な状態にし、「スレッドでメッセージを送信」も許可してください。
検知有効時には Gateway の Guild Messages / Message Content Intent を使用します。

通常の検知は本文全体をそのまま比較するため、「うお！」、「 うお」、「うお」の後ろに改行がある投稿、文章の一部に含まれる場合は対象外です。
Bot・Webhook・DM・編集イベント・過去履歴は検知しません。検知した投稿に次の形式で返信し、投稿者だけをメンションします。

```text
冷笑を検知しました！
@投稿者さんの1回目の冷笑です！
```

回数はユーザーIDごとに、全対象サーバー・チャンネル・ワードを合算します。初回は1回目です。
`/var/lib/discord-bot/counts.json` に保存するため、Bot再起動・バイナリ更新後も引き継ぎます。名前変更でも回数は変わりません。
保存が失敗した場合は加算せず通知もしません。保存後に通知だけが失敗した場合は回数を保持し、通知の再送は行いません。
既存の保存データが破損している場合は、勝手にリセットせず起動を停止します。ログを確認し、Bot停止中にバックアップから復旧してください。

設定パスと保存先は、それぞれ `DETECTION_CONFIG_PATH` と `DETECTION_STATE_PATH` で変更できます。
systemdでの保存先は既定ディレクトリ内を使用してください。それ以外の場所はサービスの書き込み制限により追加設定が必要です。
ローカル開発では書き込み可能な場所（例: `/tmp/discord-bot-dev/counts.json`）を `DETECTION_STATE_PATH` に指定します。
設定の変更は再起動時に反映されます。

```sh
sudoedit /etc/discord-bot/config.json
sudo systemctl restart discord-bot.service
```

## 定期通知

[pi-discord-server](https://github.com/HikariTakahashi/pi-discord-server) から移植した通知です。時刻は Raspberry Pi のタイムゾーンに関係なく日本時間 (JST) です。

- **毎朝 06:00:** 指定ユーザーをメンションし、YouTubeの最新動画の投稿から経過した日数（24時間単位）と動画URLを1通で送信します。RSSを取得できなかった場合は取得失敗を通知します。YouTubeのAPIキーは不要です。
- **毎週金曜 00:00:** 指定ユーザーへのメンションを約1秒間隔で50通送り、最後に「今日なにするの」と投稿します。

`/etc/discord-bot.env` に次の値を設定します。DiscordのIDは例なので、移植元の設定などから実際の値を入れてください。

```sh
DISCORD_CHANNEL_ID=123456789012345678
DISCORD_USER_ID=234567890123456789
# 省略すると移植元と同じチャンネルを使用
YOUTUBE_CHANNEL_ID=UCiTxQj4a82_B6Y0DvVAE8Ww
```

Discordの2つのIDを両方指定すると、両方の通知が有効になります。両方が未設定なら定期通知は無効になり、片方だけの指定や不正なIDは起動エラーになります。Botが通知先チャンネルを閲覧でき、メッセージを送信できることを確認してください。定期通知用の追加の Gateway Intent は不要です。

Go版の配布後に `sudo systemctl restart discord-bot.service` で設定を反映し、旧Python版をcronなどで動かしている場合は同じ通知の二重送信を避けるため停止してください。Go版の起動前や停止中に過ぎた分は後から送信しません。

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
インストーラーは検知設定を初回のみ作成します（root所有、`discord-bot` グループが読み取り可能）。既存設定・カウントは上書きしません。
systemdの `StateDirectory=discord-bot` により、Bot専用の保存ディレクトリを作成します。
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
既存環境で検知を導入するときも、先に `sudo bash scripts/install.sh` を再実行してサービス設定と初期設定ファイルを配置してください。
インストーラーは既存の `/etc/discord-bot.env` を保持するため、既定パスを使う場合は環境変数の追記は不要です。
Message Content Intent・チャンネル権限を設定した後に、検知設定へ実際のチャンネルIDを記入してBotを再起動します。
対象チャンネルで「うお」「どわー」を投稿し、通知と回数を確認してから再起動し、続きの回数になることを確認してください。
公開済みReleaseの同じタグを上書きせず、修正は新しいコミットとして配布します。古いリリースは復旧のため保持するので、ディスク容量に応じて整理してください。

## 運用

「うおw」「どわーｗｗ」のように、設定済みワードの直後に笑いの `w` が付いた投稿は、`count_laughter: true` でAI判定を有効にできます。AIが冷笑と判定した投稿だけを既存の累計に1回加算し、元の投稿に返信します。全角・大文字・連続の `w` に対応します。導入方法と、評価用のshadow収集・指定ロール専用 `/review` については [AI判定とレビューの手順](docs/shadow-review.md) を参照してください。

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
