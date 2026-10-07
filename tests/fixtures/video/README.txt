Synthetic H.264/AAC test pattern, generated locally with FFmpeg (no third-party media):
ffmpeg -f lavfi -i testsrc2=size=64x64:rate=24 -f lavfi -i sine=frequency=440:sample_rate=48000 -t 3 -c:v libx264 -preset ultrafast -g 24 -c:a aac -movflags +faststart faststart.mp4
Used to verify incremental audio/video remuxing against the bundled MP4Box implementation.
