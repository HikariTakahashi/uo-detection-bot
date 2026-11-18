# Pi上の未知変換審査と本番カウント

Macは開発・初回配置にだけ使う。稼働中はPiのGo BotがDiscordとカウントを管理し、loopbackの復元サービス、既存Laya、systemd timerが処理を進める。

## 状態と返信

`adaptive.json` の `enabled: true` で有効。未設定なら既存処理を維持する。対象チャンネル・対象語・Bot/Webhook/DM除外は既存検知設定を継承する。完全一致・既存の連続語は従来処理、それ以外は永続審査キューが排他的に担当する。

未知変換は元投稿に1件だけ返信し、加算しない。審査記録には投稿/チャンネル/サーバー/投稿者ID、本文、編集時刻、版ハッシュ、返信ID、復元機能の版と状態を保存する。原文は外部生成サービスに送信しない。審査用ディレクトリは0700、記録は0600。待機上限1000件。上限では受付失敗をログに記録し加算しない。

新しい承認済み復元リリースまたは判定方針の版を検知すると待機投稿を再取得して判定する。現在はすべての保留投稿を候補に再確認するため、無関係な復元器追加でも再確認される場合がある。復元の適用はselectorで限定する。対応しない投稿は加算せず次版を待つ。既知復元器が拒否した入力は新規生成へ自動再送しない。

返信文面は次のとおり。

```
冷笑の疑いがあるメッセージを検知しました！
<@{userId}>さんのメッセージは現在審査中です！
```

```
**再判定完了**
冷笑を検知しました！
<@{userId}>さんの{回数}回目の冷笑です！
```

```
**再判定完了**
冷笑ではありませんでした。
```

```
**審査継続**
判定が保留されました！
<@845282954636951583>、なんとかしろ！
```

再判定は同じ返信を編集する。メンション許可は指定ユーザーだけ。返信送信前にsendingを永続保存する。送信の応答が失われた場合、Bot自身の元投稿への返信を履歴から照合し、別のPOSTを自動発行しない。履歴で照合できない場合は `review reply delivery unresolved` として運用者の確認を待つ。この場合まで「必ず自動で1返信」を保証する設計ではない。手動削除されたBot返信も再作成しない。

冷笑ワード件数を累計へ反映する。投稿全体に1つのラベルを付けて全語を加算せず、対象語のある文を判定する。どの文か不確かな場合は投稿を保留する。1投稿に複数語がある場合は複数回になる。通知の `{回数}` は加算確定時点の累計。

`counts.json` はreview_receiptsを含むversion 2に移行する。累計と投稿ごとの加算結果は同じファイルへ原子的に保存する。返信編集を再試行しても再加算しない。審査ファイル更新直前のクラッシュでも、receiptから確定済みの累計を復元する。編集された投稿は最新版を取得し、以前の審査による寄与だけを取り下げて判定し直す。削除された未完了審査は取り消し、Bot返信も削除する。完了済みの履歴を削除しても確定カウントを遡って取り消す運用ではない。

## Piの各サービス

- `discord-bot.service`: Gateway、審査台帳、返信、カウント。既存Bot資格情報を保持。
- 既存user unit `laya-shadow.service`: `/adaptive/detect` と `/adaptive/judge` を追加。固定済みのオフラインモデルを使用。
- `laya-decoder.service`: 127.0.0.1:8767。生成コードはネットワークなし・読み取り専用・非rootのDockerのみで実行。Python runner内部にもメモリ/CPU上限を設定し、Piのmemory cgroup無効時を補完。
- `laya-decoder-update.timer`: 2分ごとに `/etc/laya-decoder/approved-release.json` の固定commit/indexを確認。検証成功後だけsnapshotへのsymlinkを原子的に切り替える。
- `laya-decoder-generate.timer`: 1分ごとに承認済み合成テンプレートの依頼を進める。1状態ディレクトリにつき新規依頼3件まで。POST直前にIDを保存し、応答不明時は同一IDのGETだけ。ERROR/TIMEOUTは自動再送しない。

デコーダーのtrusted controllerはdockerグループを使用するため、そのcontrollerとインストール済みソースはroot所有にする。生成コードにDocker socketや資格情報をマウントしない。Botのユーザーはdockerグループに加えない。

## 合成例・生成・公開・本番採用

無数の未知方式から独立した正解を自動で作ることはできない。初期運用は次の承認点を残す。Macは不要で、Pi上で完結する。

1. 未知形式のselectorだけを `/var/lib/laya-decoder/discovery/` に保存する。実投稿本文・ID・符号列をLunaへ送らない。
2. 運用者が公開可能な合成例と、それと独立した正常/不正入力の正解を用意し、root所有の `/etc/laya-decoder/public-templates/<family>.json` に置く。テンプレートがない未知形式は `awaiting_public_samples` のまま。
3. テンプレートの字段は `family`, `selector`, `samples`, `cases`（各text/expected）, `public_synthetic_approved: true`。復元器にはcasesの正解を与えない。
4. Luna本人性照合・コード制限・Docker内の独立検証を通過したものだけ `proposals/` に保存する。
5. Piの専用評価repo checkoutで `services/decoder/publication.py prepare` を実行する。対象はindex・contracts・registry/bundlesのみ。base commit・具体的title・全ファイルhashを `publication-plan.json` に保存する。
6. その具体案への承認後、同じJSONを `--approval` に渡して `publication.py publish` を実行する。Commit・Push後にCIが評価用Releaseを作成する。応答不明なら `publication-pending.json` の同一commitを確認し、再Commitしない。
7. 本番採用するcommit/indexを `approved-release.json` に設定する。Piのtimerが取得・ハッシュ確認・全復元器再検証後に有効化し、Botが保留投稿を自動再判定する。

GitHubの初期取得は専用repoのContents read権限だけでよい。Piから将来の承認済みCommit/Pushを実行するときは専用repoだけの書き込み資格情報とGitのcredential設定を別途用意する。BotトークンやMac Keychainの値を転用しない。

## 導入

管理者がレビュー済み `scripts/install-adaptive.sh` をPiで実行する。

1. `prepare`: docker.io/gh/git、専用ユーザー、固定ARM64イメージを準備。本番Botを変更しない。
2. `credentials`: Pi専用GitHub tokenとLuna keyを非表示入力でroot専用ファイルへ保存。systemd LoadCredentialで各サービスに渡す。
3. CIの `decoder-production.tar.gz` と `ADAPTIVE-SHA256SUMS` を固定commitから取得し検証・展開。
4. `install`: runtime/service配置、Layaアダプターのバックアップ・更新、既存11復元器の固定Release再検証。本番カウントはまだ有効化しない。
5. 合成データで復元・再判定・health・権限・復旧を確認。
6. `enable`: 既存累計をバックアップし、新Botとadaptive設定を有効化する。既存チャンネル設定・Botトークンは書き換えない。

初期復元器の固定Releaseは `38a4bbc7bc6686b71745912aee845e8e89fa2520`。復元器11件は評価済みの成果物を明示的な本番承認記録とともに利用する。評価用manifestのscopeを書き換えたり、既存Releaseを上書きしたりしない。

`enable` は更新タイマーを一時停止する。新コードのGitHub main/Releaseと導入済みcommitが一致し、起動とhealthを確認してからBot更新timerを再開する。旧版への自動更新でv2状態を読めなくなることを避ける。

停止は `disable`。adaptiveの新規受付・加算を止め、累計とreceiptを維持する。停止後も動かす旧来の検知ルートとの重複を避けるため、既存レビューreceiptを持つ投稿は従来ルートから加算しない。旧バイナリへ戻すときはv2互換性を確認し、バックアップを現在の累計にそのまま上書きしない。

## Discord DMでの本番採用承認

`install-adaptive.sh approvals` で有効化する。既存の本番版と累計を引き継ぎ、従来の固定版更新timerを停止して `laya-decoder-approval.timer` に切り替える。Botの新しいバイナリと配布ソースを先に配置する必要がある。GitHub資格情報には専用評価リポジトリの Contents read に加えて **Actions read** が必要。

Piの `publication-pushed.json` に記録されたCommit/indexだけを候補にする。専用評価workflowの最新push実行が成功し、Releaseのハッシュ・全復元器の独立検証が通った後、ユーザー `845282954636951583` のDMに「本番採用を許可／却下」のボタンを1件送る。実投稿本文は送らない。Commit・Pushや合成例登録の承認を代替しない。

回答期限は依頼作成から7日。催促は送らない。本人・DMチャンネル・Bot返信ID・依頼のハッシュを一致確認する。承認は候補Commit/indexと依頼時の本番版に固定され、候補や本番版の変更、期限切れ、却下では使えない。回答を永続化後、PiはCIと配布物を再検証し、同じ配備ロックの中で基準版の一致を確認して切り替える。成功後は既存の保留再判定を使う。DMは同じメッセージを編集し、反映完了・失敗・却下・失効を表示する。

現在版が承認済みの対象と一致し、記録が反映中なら、切り替え直後のクラッシュとして完了記録を復旧する。旧初期承認ファイルは定期処理で再適用しない。取得などの一時失敗は同じ承認だけで再試行し、検証不一致では旧版を保持して停止する。

承認連携は `/var/lib/laya-approvals` 配下だけを専用グループで共有する。requests/resultsはlaya-decoderのみ書き込み、decisionsはdiscord-botのみ書き込む。BotのDM送信記録は `/var/lib/discord-bot/release-approvals` に0700/0600で保存する。BotにsudoやDocker権限を追加しない。

DM拒否時はblocked記録を保持し、公開チャンネルへ転送しない。POST応答不明はBot自身のDM履歴を照合し、見つからなければ再POSTせず運用者確認を待つ。手動で消されたDMも自動再作成しない。却下・失効済みの同じ候補は再通知しない。再依頼が必要な場合は、通常のapprovalサービスと同じユーザー・資格情報・環境で `approval.py` に `--retry-request <終了済み依頼ID>` を指定する。再検証後に新しい依頼ID・期限で作成する。

Piで確認した配備条件として、3つのdecoderサービスのTMPDIRは書き込み可能な専用StateDirectoryを使う。runtime制御プロセスの仮想アドレス上限はDocker CLIが起動できる2GiB、生成コード内部のアドレス上限は128MiBを維持する。Lunaリレーへの通信はアプリ名のUser-Agent `uo-decoder-pi/1.0` を明示する。
