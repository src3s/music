<div align="center">

**Single track**

![demo](assets/demo.gif)

**Playlist**

![Playlist Demo](assets/playlist-demo.gif)

</div>

## Install

**Windows**
```powershell
irm https://ohcass.github.io/music/install.ps1 | iex
```

**Linux / macOS**
```sh
curl -fsSL https://raw.githubusercontent.com/ohcass/music/main/scripts/install.sh | sh
```

<details>
<summary>Build from source</summary>

```sh
git clone https://github.com/ohcass/music.git
cd music
go build -o radii5 ./cmd/music
```
</details>

## Usage

```sh
radii5 <url>                                          # default: mp3 audio
radii5 --type video <url>                             # download as mp4 video
radii5 --type video --quality 720 <url>               # 720p video
radii5 --mp4 720 <url>                                # shorthand: 720p video
radii5 --mp4 <url>                                    # video at default quality (1080)
radii5 <url> --format flac                            # audio format
radii5 "https://youtube.com/playlist?list=..."        # playlist (audio)
radii5 --mp4 "https://youtube.com/playlist?list=..."  # playlist (video)
radii5 <url> --threads 16 --workers 6                 # tune performance
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--format`, `-f` | `mp3` | Audio format: `mp3`, `flac`, `m4a`, `opus`, `wav` |
| `--output`, `-o` | `~/Music/radii5 downloads` | Output directory |
| `--threads`, `-t` | `8` | Parallel download chunks |
| `--workers`, `-w` | `4` | Concurrent playlist download workers |
| `--type` | `audio` | Media type: `audio` or `video` |
| `--quality`, `-q` | `1080` | Video height: `144`, `240`, `360`, `480`, `720`, `1080`, `1440`, `2160` |
| `--mp4` | — | Shortcut for `--type video`. Optionally followed by quality (e.g. `--mp4 720`) |

## Features

- **Audio download**: Downloads best available audio, converts to MP3 (LAME V2) with metadata tags
- **Video download**: Downloads best video+audio up to requested quality, merges to MP4
- **Playlist support**: Concurrent playlist downloads with per-track progress, retry logic, smooth animations
- **Fast parallel downloads**: Chunked HTTP downloads with byte-level progress tracking
- **Cross-platform**: Windows, Linux, macOS

## Requirements

- [yt-dlp](https://github.com/yt-dlp/yt-dlp) (bundled with installer)
- [ffmpeg](https://ffmpeg.org/) (bundled with installer)

## License

MIT
