# Laya Multilingual 冷笑分類の独立試験

既存のGo製Discord Botとは別に、手入力した文章だけを分類する実験です。
Discordへの接続、投稿取得、メンション、カウントの読み書き、Botの再起動は行いません。
Botの自動配布にも組み込んでいません。

## Piで試す

設置先は `uo-ai` の `/home/user/laya-sandbox` です。
MacからSSHで入った後、次のように起動します。

```sh
ssh uo-ai
cd /home/user/laya-sandbox
./run.sh
```

`投稿>` に文章を1行ずつ入力すると結果が表示されます。
終了は `/quit` または Ctrl-D。初回のモデル読み込み後は同じプロセスで繰り返し試せます。
入力はファイルへ自動保存しません。端末の表示・スクロールバックには残ります。

1件だけ、または付属の合成例文を試す場合:

```sh
./run.sh --text 'うお！優勝おめでとう！'
./run.sh --text 'うお' --context '返信元の文章'
./run.sh --examples
```

判定指示は `prompts.json` に分離しています。旧版や比較候補を選んで試せます。

```sh
./run.sh --prompt baseline --text 'うお、また自称天才のご登場ですか。'
./run.sh --prompt ja_rules --text 'うお、また自称天才のご登場ですか。'
./run.sh --prompt en_rules --text 'うお、また自称天才のご登場ですか。'
./run.sh --prompt ja_examples --text 'うお、また自称天才のご登場ですか。'
```

旧版 `baseline` は元の指示文をそのまま保持しています。
今回の6案の比較では総合改善を確認できず、既定も `baseline` を維持しています。
追加候補 `en_reactions`、`ja_focus`、`en_literal` も `--prompt` で選べます。
各出力の `prompt` と `prompt_sha256` で、どの指示文で判定したか確認できます。
日本語の文章を英語の指示文で判定する場合も、本文自体は翻訳しません。

固定例文での比較を再実行する場合:

```sh
./run.sh --evaluate eval-dev.jsonl --compare baseline ja_rules en_rules ja_examples
```

最後に各指示文の正解数・誤検知数・冷笑の再現率・保留数などがJSONで表示されます。
評価用の正解ラベルはモデルへ渡しません。
今回の選考方法と開発例文での結果は `PROMPT_EVALUATION.md` に記録しています。
採用基準を満たす候補がなかったため、確認用24件はまだ実行していません。

`--text` のコマンド引数はシェル履歴に残る場合があります。
継続して試す場合は引数なしの対話モードを使ってください。

「対象ワード＋w」の専用指示は、以下の合成例で試せます。`target_words` はBotが候補として抽出したワードです。`label` と `reference_label` を比較してください。実投稿の見逃し例・引用例でも確認してから加算を有効にします。

```sh
./run.sh --prompt laughter --batch laughter-examples.jsonl
./run.sh --prompt laughter --target-words 'うお' --text 'うおｗｗ'
```

[Piの実モデルでの検証結果](LAUGHTER_VALIDATION.md)では、引用・説明の前処理を追加した版が合成例11件すべてで期待どおりに動作しました。

繰り返し候補に通常AI判定を使わない判断を確認するため、次の固定合成例を実モデルで評価できます。`uououo` 自体だけでは意図を決められないため、本文は同じにして返信元だけを変え、冷笑・対象外・判断困難を比較します。正解ラベルはモデルに渡しません。

```sh
./run.sh --evaluate repetition-eval.jsonl --compare baseline
```

この機能はレビュー専用です。評価結果は [REPETITION_VALIDATION.md](REPETITION_VALIDATION.md) に記録しています。モデルの結果を自動加算の根拠にはしません。評価では対象外を冷笑とした件数、冷笑の検出数、保留数を分けて記録してください。

## 結果の読み方

- `label: mockery` — 冷笑という仮判定。
- `label: other` — 対象外という仮判定。
- `label: uncertain` — 文脈不足などで判断困難。入力の切り捨てを検出した場合も保留。
- `source: url_filter` — URLを除いた本文が空だったため、AIを呼ばずに対象外。
- `source: laughter_filter` — 引用・コード・説明の用法を除いた後に「対象ワード＋w」が残らず、AIを呼ばずに対象外。
- `source: laya` — モデルが分類した結果。
- `answer` — 選択肢 A=冷笑、B=対象外、C=判断困難と、モデルの生の出力。
- `seconds` — その投稿の推論時間。モデル読み込み時間は `load_seconds` に別記。

確率やconfidenceは未校正です。たとえば0.9を正答率90%と解釈しないでください。
`examples.jsonl` の `reference_label` は合成例文に付けた暫定正解で、
サーバー内の取り決めや実投稿による精度評価を代替しません。

## 分離と資源制限

専用のPython仮想環境・モデル保存先を使います。モデルを取得した後はオフラインで実行します。
HTTPサーバー、systemd常駐サービス、自動起動設定は作成しません。
`run.sh` はsystemdのユーザースコープでCPUを1コア相当までに制限し、
nice値10で動作します。CPUスレッドは2本です。
このPiではmemory cgroupが無効なので、代わりに `ulimit -v` でプロセスの
仮想アドレス空間を4GiBまでに制限します。これは実メモリ4GiBの割当ではなく、
マッピングされたファイルなども含む上限です。スワップ使用を禁止する設定ではありません。
カーネル設定変更やPiの再起動は行っていません。
`flock` により、このランチャーからの同時起動は1つに制限します。
資源上限の設定に失敗した場合、制限なしでの実行へは切り替えません。
同じPiで動くため、これらの上限内でCPU・メモリ・ストレージ帯域を使用します。

通常は必ず `run.sh` を使ってください。`classify.py` を直接起動するとsystemdの資源上限は付きません。
入力は本文と返信元を合わせて2000文字まで、モデルの入力上限は1024トークンです。
URL除外は実験用の簡易処理で、すべてのDiscord MarkdownやURL表現を網羅していません。
表記揺れの完全な候補抽出や、既存Botの検知ワード設定との同期も実装していません。

## 固定したモデル

- パッケージ: `laya==0.4.31`
- CPU実行: `torch==2.9.1+cpu`
- モデル: `convaiinnovations/laya-multilingual`
- モデルrevision: `1720e3e3357cfe1e281542e223f8273b0890ca34`

英語版LayaやVegaへ自動で切り替えず、指定したMultilingualだけを読み込みます。
実際に検証した依存関係は `requirements.lock.txt` を参照してください。

## 再構築

Linux ARM64、Python 3.13とvenv、ユーザーsystemd、flockがある環境を想定しています。
このディレクトリのソースを専用の書き込み可能な場所へコピーして実行します。
既存Botの環境やシステムPythonへのインストールは不要です。

```sh
python3 -m venv .venv
PIP_CONFIG_FILE=/dev/null PIP_EXTRA_INDEX_URL= .venv/bin/python -m pip install \
  --index-url https://download.pytorch.org/whl/cpu 'torch==2.9.1+cpu'
PIP_CONFIG_FILE=/dev/null PIP_INDEX_URL=https://pypi.org/simple PIP_EXTRA_INDEX_URL= \
  .venv/bin/python -m pip install -r requirements.lock.txt
.venv/bin/python download_model.py
chmod 700 run.sh
./run.sh --examples
```

モデル取得だけはインターネットを使います。公開モデルの取得に個人の認証トークンは使いません。
判定する投稿をモデル配布元へ送信しません。

基本テスト（AIモデルなしで実行可能）:

```sh
python3 -m unittest discover -s . -p 'test_*.py' -v
```

削除する場合は対話モードを終了し、専用の `laya-sandbox` ディレクトリを削除します。
Botの設定変更やサービス再起動は不要です。
