# 冷笑のAI判定とレビュー

設定の `mode: "shadow"` はAIの評価用収集を有効にします。レビュー操作は通知・カウントを変更しません。`count_laughter: true` を設定すると「対象ワード＋w」のAI判定を通知・加算に使えます。設定例では両方とも無効です。旧設定に `count_laughter` がなく、`mode: "shadow"` と `use_detection_channels: true` を併用している環境では、更新時にAI加算が有効になります。レビュー用の収集だけを続けるには `count_laughter: false` を指定してください。

## 対象ワード＋w をカウントする

- `config.json` の対象チャンネルとワードを使用します。英字の大小・全角半角・ひらがなとカタカナを揃えて、対象ワードの直後に `w` が1つ以上ある候補を探します。`うおw`、`ウオＷｗ！`、`どわーw それは無理でしょ` が対象です。
- 引用符内、Discordの引用行、コード、語の意味や表記を説明する用法を本文の候補から除き、残った候補だけをAIに渡します。投稿者自身がこの用法で使っていれば、悪口が書かれていなくても冷笑と判定するよう指示します。URLの一部や返信元だけの用法は対象外です。
- AIが冷笑と判定した投稿だけ加算・返信します。加算済みの投稿IDを累計と一緒に保存するので、Bot再起動後も同じ投稿を二重に加算しません。対象ワードだけの投稿は従来の完全一致で扱います。
- AIは1件ずつ処理し、評価用のshadowより優先します。本文は2,000文字まで、待ち行列は20件、受付からAI判定まで最長30秒です。サービス停止・タイムアウト時は期限内に1回再試行します。枠が埋まった場合や判定に失敗した場合はログに出し、加算しません。判定を保留した投稿は `/review` に回します。レビュー結果による加算はありません。

## 連続した冷笑ワードを検知する

`uououo` や `うおうお` のような連続した本文は、通常の検知として1投稿につき1回だけ加算・返信します。`mode`・`count_laughter`・AIサービスの稼働状態には依存しません。

- 対象は、設定済みワードのうち英字の大小・全角英数・カタカナを正規化して `uo` または `うお` になるものだけです。本文全体がこれらの2個以上の連結なら検知します。両方が登録済みなら `uoＵＯウオ` のような混在も対象です。
- 判定には加工前の本文を使い、URL・引用・空白・改行・句読点を除去して一致させることはしません。`uo uo`、`「uouo」`、`uouo！`、`uouo` にURLが続く投稿は対象外です。`ワオワオ`、`♓♓`、`uoどわー`、小書きの `うぉうぉ`、半角カナの `ｳｵｳｵ` も対象外です。
- AI判定やshadow保存は行いません。対象チャンネル、Bot・Webhook・DM・投稿長の扱いは通常の完全一致検知と同じです。

既存の完全一致設定は優先します。例えば `uouo` 自体を `words` に登録済みなら、従来どおり完全一致で加算します。`uououow` など笑い付きの繰り返しは今回の追加対象ではありません。

## 収集するもの

`shadow.json` の `use_detection_channels: true` で既存 `config.json` の検知対象チャンネルすべてを引き継ぎます。設定変更はBot再起動時に反映します。Bot・Webhook・DMは対象外です。チャンネルに紐づくスレッドは、そのスレッドIDが既存の対象一覧にある場合に限ります。

- URLを除去し、英字の大小・全角英数・カタカナを正規化して、既存検知ワードを部分一致で探します。候補はすべて収集対象です。「ちゅううおう」も誤判定を確認するため候補に含めます。
- それ以外は投稿IDのハッシュで約10件に1件を抽出します。URLだけの投稿もこの抽出対象で、AIを呼ばず `other / url_filter` として保存します。
- 本文と、Gatewayイベントに既に含まれる同じチャンネルの返信元だけを使います。履歴の追加取得はしません。本文2,000文字を超える投稿は収集しません。返信元との合計が超える場合は返信元を付けません。
- 待ち行列は10件、推論は1件ずつ、受付から応答まで最大10秒です。混雑時は収集を間引きます。判定エラーはエラーとして保存し、正解率の計算対象に混ぜません。

保存先は `/var/lib/discord-bot/shadow/<投稿ID>.json`。フォルダー0700、ファイル0600です。本文、返信元、投稿・チャンネル・サーバーID、収集日時、抽出区分・抽出率、従来の完全一致結果、Bot版、AIのラベル・確率・モデル版・指示文ハッシュ・処理時間、レビュー担当者の正解ラベル・理由・IDを保存します。投稿者ID・名前は保存しません。本文に書かれた個人情報は本文の一部として残ります。

最大1,000件、レビュー済みを含め収集から7日で削除します。期限は起動時と毎分確認します（Bot停止中は次回起動時）。満杯時は新規保存を停止します。投稿削除イベントを受け取った場合、その投稿と、それを返信元として保存したデータも削除します。Bot停止中に発生した削除は取得できないため、残存分は7日間の期限で削除します。編集イベントでスナップショットを更新しないため、レビューするのは収集時の本文です。

ファイル破損・権限異常で保存先を開けない場合はshadowを無効にし、ログへエラーを記録します。復旧まで自動整理も実行できないため、管理者が保存先を確認してください。BotがDiscordへ返した一時表示の本文は、このローカル保存先の削除とは別の扱いです。

## /review の使い方

ロール ID `1023596331392053388` を持つメンバーが、サーバー内で `/review` を実行できます。Bot はコマンド実行・ボタン操作・入力送信のたびにロールを確認し、ロールを持たないメンバーは管理者でも操作できません。返答は実行者だけに表示されます。

Discord のコマンド候補にも表示制限をかけたい場合は、[サーバー設定の「連携サービス」で `/review` の権限](https://support-apps.discord.com/hc/en-us/articles/26501869403159-Command-Permissions)を設定してください。管理者には Discord の仕様上、コマンド候補が表示されます。

1. AIの答えを伏せた本文・返信元を読みます。
2. 画面に表示される基準に沿って「冷笑」「対象外」「判断困難」を選びます。「対象ワード＋w」の候補と、通常の評価用サンプルでは基準が異なります。
3. 任意で理由を200文字以内で入力し、送信します。
4. 正解をファイルへ保存できた後にAIの答えを表示します。「次の未レビュー」で続けます。

「今回はスキップ」はそのレビュー担当者に対して15分間保留します。「データを削除」は評価用データのみ削除します。Discordの元投稿・既存カウントは変えません。同じ投稿を複数の担当者に同時提示せず、画面は15分で失効します。再起動後は `/review` を開き直してください。ラベル保存は1回限りです。AIが失敗したデータはレビュー一覧に出しません。

比較では候補と非候補を分け、冷笑の見逃し・誤検知・判断困難をそれぞれ集計してください。候補と非候補で抽出率が違うため、単純な全件正答率は全投稿の正答率にはなりません。モデルの確率は未校正で、正答率ではありません。

## Raspberry Piで有効にする

前提は既に導入済みの `/home/user/laya-sandbox`（Laya 0.4.31、固定モデル版、CPU版PyTorch）です。新しいBotのリリースにはGoの変更を含める必要があります。通常のRelease手順で配布してください。Laya API・unit・設定はGoバイナリの自動配布には含まれません。

Piにリポジトリを配置した状態で、一般ユーザー `user` として次を実行します。

```sh
install -m 0644 experiments/laya/serve_shadow.py experiments/laya/classify.py \
  experiments/laya/prompts.json "$HOME/laya-sandbox/"
mkdir -p "$HOME/.config/systemd/user"
install -m 0644 deploy/laya-shadow.service "$HOME/.config/systemd/user/laya-shadow.service"
systemctl --user daemon-reload
systemctl --user enable --now laya-shadow.service
systemctl --user restart laya-shadow.service
# SSH切断後・再起動後もユーザーサービスを動かす設定（初回のみ）
sudo loginctl enable-linger user
curl --fail http://127.0.0.1:8766/health
```

初回のモデル読込に数秒かかるため、healthが接続拒否ならログを確認して再試行します。CPUは1コア相当・Nice 10、プロセス全体のアドレス空間は4GiBに制限します。このPiはmemory cgroupが無効なので、RAM使用量制限とは異なります。APIは127.0.0.1のみで、オフラインの同じモデルを使い、同時推論は1件です。

加算を有効にする前に、PiのローカルAPIへ合成例を渡して `prompt: "laughter"` とラベルを確認できます。このリクエストでDiscordへの返信や加算は発生しません。過去の見逃し例も同じ方法で確認してください。

```sh
curl --fail --header 'Content-Type: application/json' \
  --data '{"text":"うおｗｗ","target_words":"うお"}' \
  http://127.0.0.1:8766/classify/laughter
curl --fail --header 'Content-Type: application/json' \
  --data '{"text":"「うおw」は検知されますか？","target_words":"うお"}' \
  http://127.0.0.1:8766/classify/laughter
```

期待値は順に `mockery` と `other` です。試験用 `run.sh` で確認する場合は、同じロックを使うため先にLayaサービスを止めてください。

新しいGo版の配布とAPIの確認後、設定を変更します。既存の検知設定・counts.json・トークンは手動編集しません。

```sh
# 初回のみ。既にshadow設定があればコピーせず、次のsudoeditで編集します。
sudo install -o root -g discord-bot -m 0640 shadow.config.example.json /etc/discord-bot/shadow.json
sudoedit /etc/discord-bot/shadow.json
# AIで加算する場合は count_laughter を true に変更。
# 評価用収集も行う場合は mode を "shadow" に変更。
sudo systemctl restart discord-bot.service
systemctl status discord-bot.service
systemctl --user status laya-shadow.service
```

`/review` は既存 `/ping` と同じサーバーまたはグローバルの範囲に追加登録されます。グローバル登録の表示反映には時間差があります。コマンド登録失敗はログに記録しますが、既存Botを停止しません。新しい実投稿で自然に収集されるまで、未レビュー0件の表示は正常です。動作確認のために検知ワードを実チャンネルへ投稿すると従来どおりカウントされるので、テストは合成データと一時ディレクトリで実施します。

## 停止・確認

```sh
sudoedit /etc/discord-bot/shadow.json
# mode を "off"、count_laughter を false に変更
sudo systemctl restart discord-bot.service
systemctl --user disable --now laya-shadow.service
```

収集を止めても保存済みデータのレビューと期限による整理は継続します。設定ファイルがなければAIによる加算・収集は無効です。Layaが停止していても完全一致による通知・加算は継続します。

最初のAI加算までは従来の累計ファイル形式を維持します。AI加算後に機能を停止する場合は、設定で `count_laughter` を無効にしてください。旧バイナリへの切り戻しが必要な場合、旧版は加算済み投稿IDを含む新形式を読めません。Botを止めて元ファイルをバックアップし、新形式の `counts` オブジェクトを旧版の累計ファイルとして取り出してください。

```sh
journalctl -u discord-bot.service -n 50
journalctl --user -u laya-shadow.service -n 50
go test -race ./...
go vet ./...
python3 -m unittest discover -s experiments/laya -p 'test_*.py' -v
```

本文・返信元・推論の生のエラーメッセージを運用ログへ出さず、収集落ちの件数と汎用エラーだけを記録します。保存先が満杯・権限異常の場合も既存Botは継続します。
