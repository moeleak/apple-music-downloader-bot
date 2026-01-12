[English](./README.md) / 简体中文

### ！！必须先安装[MP4Box](https://gpac.io/downloads/gpac-nightly-builds/)，并确认[MP4Box](https://gpac.io/downloads/gpac-nightly-builds/)已正确添加到环境变量

### 添加功能

1. 支持内嵌封面和LRC歌词（需要`media-user-token`，获取方式看最后的说明）
2. 支持获取逐词与未同步歌词
3. 支持下载歌手 `go run main.go https://music.apple.com/us/artist/taylor-swift/159260351` `--all-album` 自动选择歌手的所有专辑
4. 下载解密部分更换为Sendy McSenderson的代码，实现边下载边解密,解决大文件解密时内存不足
5. MV下载，需要安装[mp4decrypt](https://www.bento4.com/downloads/)

### 特别感谢 `chocomint` 创建 `agent-arm64.js`
对于获取`aac-lc` `MV` `歌词` 必须填入有订阅的`media-user-token`

- `alac (audio-alac-stereo)`
- `ec3 (audio-atmos / audio-ec3)`
- `aac (audio-stereo)`
- `aac-lc (audio-stereo)`
- `aac-binaural (audio-stereo-binaural)`
- `aac-downmix (audio-stereo-downmix)`
- `MV`

# Apple Music ALAC/杜比全景声下载器

原脚本由 Sorrow 编写。本人已修改，包含一些修复和改进。

## 使用方法
1. 确保解密程序 [wrapper](https://github.com/WorldObservationLog/wrapper) 正在运行
2. 开始下载部分专辑：`go run main.go https://music.apple.com/us/album/whenever-you-need-somebody-2022-remaster/1624945511`。
3. 开始下载单曲：`go run main.go --song https://music.apple.com/us/album/never-gonna-give-you-up-2022-remaster/1624945511?i=1624945512` 或 `go run main.go https://music.apple.com/us/song/you-move-me-2022-remaster/1624945520`。
4. 开始下载所选曲目：`go run main.go --select https://music.apple.com/us/album/whenever-you-need-somebody-2022-remaster/1624945511` 输入以空格分隔的数字。
5. 开始下载部分播放列表：`go run main.go https://music.apple.com/us/playlist/taylor-swift-essentials/pl.3950454ced8c45a3b0cc693c2a7db97b` 或 `go run main.go https://music.apple.com/us/playlist/hi-res-lossless-24-bit-192khz/pl.u-MDAWvpjt38370N`。
6. 对于杜比全景声 (Dolby Atmos)：`go run main.go --atmos https://music.apple.com/us/album/1989-taylors-version-deluxe/1713845538`。
7. 对于 AAC (AAC)：`go run main.go --aac https://music.apple.com/us/album/1989-taylors-version-deluxe/1713845538`。
8. 要查看音质：`go run main.go --debug https://music.apple.com/us/album/1989-taylors-version-deluxe/1713845538`。

[中文教程-详见方法三](https://telegra.ph/Apple-Music-Alac高解析度无损音乐下载教程-04-02-2)

## Telegram 机器人模式
1. 在 `config.yaml` 设置 `telegram-bot-token`（或导出 `TELEGRAM_BOT_TOKEN`）。
2. 可选：设置 `telegram-allowed-chat-ids` 限制使用者。
3. 启动：`go run main.go --bot`
4. 命令示例：
   - `/search_song <关键词>`
   - `/search_album <关键词>`
   - `/search_artist <关键词>`
   - `/id <song|album> <id>`

注意：
- 机器人会发送 FLAC，需要系统有 `ffmpeg`。
- 超过限制的文件会重新压缩到 `telegram-max-file-mb`（音质可能下降）。
- 如需中文搜索结果，可设置 `telegram-search-language`（例如 `zh-Hans`）或全局 `language`。
- 如需“秒传”复用 Telegram 缓存，可设置 `telegram-cache-file` 保存 file_id 缓存。

## 下载歌词

1. 打开 [Apple Music](https://music.apple.com) 并登录
2. 打开开发者工具，点击“应用程序 -> 存储 -> Cookies -> https://music.apple.com”
3. 找到名为“media-user-token”的 Cookie 并复制其值
4. 将步骤 3 中获取的 Cookie 值粘贴到 config.yaml 文件中并保存
5. 正常启动脚本
