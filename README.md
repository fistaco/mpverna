# MPVerna
MPVerna is a web application that aids your language learning journey by streaming MPV's subtitles to your browser, where you can then use a pop-up dictionary browser extension like [Yomitan](https://yomitan.wiki/) to view translations and definitions for those subtitles.

Subtitles are generated using Whisper if MPVerna fails to detect an existing subtitle track in the desired language.

# Prerequisites
MPVerna depends on these projects for its full functionality:
- For subtitle generation, [Whisper](https://github.com/openai/whisper) has to be installed and available in your `PATH`; 
- For conversion from embedded subtitles to an SRT file, [FFmpeg](https://www.ffmpeg.org/download.html) has to be installed and available in your `PATH`.

# Usage
Install MPVerna:
```bash
git clone git@github.com:fistaco/mpverna.git && cd mpverna
```

Start MPVerna:
```bash
go run .
```

Start your video file:
```bash
mpv --input-ipc-server=\\.\pipe\mpv-subs video.mkv
```

At the time of writing, MPVerna has only been tested on Windows.
