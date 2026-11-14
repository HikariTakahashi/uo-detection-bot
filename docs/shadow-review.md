# shadow収集と管理者レビュー

既存Botの完全一致による通知・加算と、AIの評価用データ収集を別々に実行します。AIの判定・停止・タイムアウト・レビュー結果は、通知とカウントを変更しません。AIによる自動通知モードは実装していません。

## 収集するもの

`shadow.json` の `use_detection_channels: true` で既存 `config.json` の検知対象チャンネルすべてを引き継ぎます。設定変更はBot再起動時に反映します。Bot・Webhook・DMは対象外です。チャンネルに紐づくスレッドは、そのスレッドIDが既存の対象一覧にある場合に限ります。

- URLを除去し、英字の大小・全角英数・カタカナを正規化して、既存検知ワードを部分一致で探します。候補はすべて収集対象です。「ちゅううおう」も誤判定を確認するため候補に含めます。
- それ以外は投稿IDのハッシュで約10件に1件を抽出します。URLだけの投稿もこの抽出対象で、AIを呼ばず `other / url_filter` として保存します。
- 本文と、Gatewayイベントに既に含まれる同じチャンネルの返信元だけを使います。履歴の追加取得はしません。本文2,000文字を超える投稿は収集しません。返信元との合計が超える場合は返信元を付けません。
- 待ち行列は10件、推論は1件ずつ、受付から応答まで最大10秒です。混雑時は収集を間引きます。判定エラーはエラーとして保存し、正解率の計算対象に混ぜません。

保存先は `/var/lib/discord-bot/shadow/<投稿ID>.json`。フォルダー0700、ファイル0600です。本文、返信元、投稿・チャンネル・サーバーID、収集日時、抽出区分・抽出率、従来の完全一致結果、Bot版、AIのラベル・確率・モデル版・指示文ハッシュ・処理時間、管理者の正解ラベル・理由・IDを保存します。投稿者ID・名前は保存しません。本文に書かれた個人情報は本文の一部として残ります。

最大1,000件、レビュー済みを含め収集から7日で削除します。期限は起動時と毎分確認します（Bot停止中は次回起動時）。満杯時は新規保存を停止します。投稿削除イベントを受け取った場合、その投稿と、それを返信元として保存したデータも削除します。Bot停止中に発生した削除は取得できないため、残存分は7日間の期限で削除します。編集イベントでスナップショットを更新しないため、レビューするのは収集時の本文です。

ファイル破損・権限異常で保存先を開けない場合はshadowを無効にし、ログへエラーを記録します。復旧まで自動整理も実行できないため、管理者が保存先を確認してください。BotがDiscordへ返した一時表示の本文は、このローカル保存先の削除とは別の扱いです。

## /review の使い方

サーバー管理者が `/review` を実行します。返答は実行者だけに表示されます。管理者権限はコマンドだけでなく、すべてのボタン・入力送信時にも検査します。

1. AIの答えを伏せた本文・返信元を読みます。
2. 「冷笑」「対象外」「判断困難」を選びます。相手を見下す・嘲る使い方は冷笑、引用・説明・驚き・単語の一部は対象外、文脈が足りない場合は判断困難です。
3. 任意で理由を200文字以内で入力し、送信します。
4. 正解をファイルへ保存できた後にAIの答えを表示します。「次の未レビュー」で続けます。

「今回はスキップ」はその管理者に対して15分間保留します。「データを削除」は評価用データのみ削除します。Discordの元投稿・既存カウントは変えません。複数管理者へ同じ投稿を同時提示せず、画面は15分で失効します。再起動後は `/review` を開き直してください。ラベル保存は1回限りです。AIが失敗したデータはレビュー一覧に出しません。

比較では候補と非候補を分け、冷笑の見逃し・誤検知・判断困難をそれぞれ集計してください。候補と非候補で抽出率が違うため、単純な全件正答率は全投稿の正答率にはなりません。モデルの確率は未校正で、正答率ではありません。

## Raspberry Piで有効にする

前提は既に導入済みの `/home/user/laya-sandbox`（Laya 0.4.31、固定モデル版、CPU版PyTorch）です。新しいBotのリリースにはGoの変更を含める必要があります。通常のRelease手順で配布してください。Laya API・unit・設定はGoバイナリの自動配布には含まれません。

Piにリポジトリを配置した状態で、一般ユーザー `user` として次を実行します。

```sh
install -m 0644 experiments/laya/serve_shadow.py "$HOME/laya-sandbox/serve_shadow.py"
mkdir -p "$HOME/.config/systemd/user"
install -m 0644 deploy/laya-shadow.service "$HOME/.config/systemd/user/laya-shadow.service"
systemctl --user daemon-reload
systemctl --user enable --now laya-shadow.service
# SSH切断後・再起動後もユーザーサービスを動かす設定（初回のみ）
sudo loginctl enable-linger user
curl --fail http://127.0.0.1:8766/health
```

初回のモデル読込に数秒かかるため、healthが接続拒否ならログを確認して再試行します。CPUは1コア相当・Nice 10、プロセス全体のアドレス空間は4GiBに制限します。このPiはmemory cgroupが無効なので、RAM使用量制限とは異なります。APIは127.0.0.1のみ、モデルはオフライン・baseline固定、同時推論1件です。試験用 `run.sh` と同じロックを使うため、手動試験の際はサービスを止めてください。

新しいGo版の配布とhealth成功を確認してから、以下で収集を有効にします。既存の検知設定・counts.json・トークンは編集しません。

```sh
# 初回のみ。既にshadow設定があればコピーせず、次のsudoeditで編集します。
sudo install -o root -g discord-bot -m 0640 shadow.config.example.json /etc/discord-bot/shadow.json
sudoedit /etc/discord-bot/shadow.json
# mode を "shadow" に変更。use_detection_channels は true のまま。
sudo systemctl restart discord-bot.service
systemctl status discord-bot.service
systemctl --user status laya-shadow.service
```

`/review` は既存 `/ping` と同じサーバーまたはグローバルの範囲に追加登録されます。グローバル登録の表示反映には時間差があります。コマンド登録失敗はログに記録しますが、既存Botを停止しません。新しい実投稿で自然に収集されるまで、未レビュー0件の表示は正常です。動作確認のために検知ワードを実チャンネルへ投稿すると従来どおりカウントされるので、テストは合成データと一時ディレクトリで実施します。

## 停止・確認

```sh
sudoedit /etc/discord-bot/shadow.json
# mode を "off" に変更
sudo systemctl restart discord-bot.service
systemctl --user disable --now laya-shadow.service
```

offでも保存済みデータのレビューと期限による整理は継続します。設定ファイルがない場合もoffです。Layaが停止していても既存通知・加算は継続します。

```sh
journalctl -u discord-bot.service -n 50
journalctl --user -u laya-shadow.service -n 50
go test -race ./...
go vet ./...
python3 -m unittest discover -s experiments/laya -p 'test_*.py' -v
```

本文・返信元・推論の生のエラーメッセージを運用ログへ出さず、収集落ちの件数と汎用エラーだけを記録します。保存先が満杯・権限異常の場合も既存Botは継続します。
