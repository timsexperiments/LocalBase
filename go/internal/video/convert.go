package video

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The AVI to MP4 converter (TS video-converter.ts): only the private AVI the
// backend produced is converted, by the pinned ffmpeg, with a fixed command,
// a 120 s deadline, bounded output, and a frame count that must match.

const (
	conversionTimeout = 120 * time.Second
	diagnosticBytes   = 4096
)

var progressFrame = regexp.MustCompile(`^frame=\d+$`)

// Converter prepares artifacts with the managed ffmpeg.
type Converter struct {
	// Ensure returns the pinned ffmpeg's absolute path, installing it once.
	Ensure func(ctx context.Context) (string, error)
	// Timeout overrides the deadline (tests).
	Timeout time.Duration
}

// Prepare converts media to MP4 (Options.Prepare).
func (c *Converter) Prepare(ctx context.Context, media Media, dir string, maxBytes int64) (Media, error) {
	if err := ctx.Err(); err != nil {
		return Media{}, err
	}
	b := media.Data
	if media.Format != "avi" || len(b) < 12 || int64(len(b)) > maxBytes || string(b[:4]) != "RIFF" || string(b[8:12]) != "AVI " {
		return Media{}, errors.New("Video conversion requires a bounded AVI artifact.")
	}
	timeout := conversionTimeout
	if c.Timeout > 0 {
		timeout = c.Timeout
	}
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, errors.New("Video conversion timed out."))
	defer cancel()
	exe, err := c.Ensure(ctx)
	if err != nil {
		return Media{}, err
	}
	if err := ctx.Err(); err != nil {
		return Media{}, context.Cause(ctx)
	}
	if !filepath.IsAbs(exe) {
		return Media{}, errors.New("Video converter must use a managed absolute path.")
	}
	tmp, err := os.MkdirTemp(dir, "convert-")
	if err != nil {
		return Media{}, err
	}
	defer os.RemoveAll(tmp)
	os.Chmod(tmp, 0o700)
	in, out := filepath.Join(tmp, "input.avi"), filepath.Join(tmp, "output.mp4")
	if err := writePrivate(in, b); err != nil {
		return Media{}, err
	}
	if err := writePrivate(out, nil); err != nil {
		return Media{}, err
	}
	cmd := exec.Command(exe, "-nostdin", "-hide_banner", "-loglevel", "error", "-nostats", "-y",
		"-progress", "pipe:1", "-filter_threads", "2", "-filter_complex_threads", "2",
		"-protocol_whitelist", "file", "-threads", "2", "-f", "avi", "-i", in,
		"-map", "0:v:0", "-map", "0:a:0?", "-map_metadata", "-1", "-map_chapters", "-1",
		"-c:v", "libx264", "-threads:v", "2",
		// Convert samples as well as marking the range; MJPEG may be full-range.
		"-vf", "scale=out_range=tv", "-color_range", "tv", "-pix_fmt", "yuv420p",
		"-preset", "veryfast", "-crf", "23", "-c:a", "aac", "-threads:a", "2", "-b:a", "128k",
		"-movflags", "+faststart", "-fs", strconv.FormatInt(maxBytes, 10),
		"-protocol_whitelist", "file", "-f", "mp4", out)
	cmd.Dir = tmp
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Media{}, err
	}
	// Native diagnostics can contain media data: drained, never kept or logged.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return Media{}, err
	}
	type result struct {
		frames int
		err    error
	}
	progress := make(chan result, 1)
	go func() {
		frames, err := readProgress(stdout)
		progress <- result{frames, err}
	}()
	exited := make(chan error, 1)
	go func() {
		p := <-progress
		werr := cmd.Wait()
		if p.err == nil {
			p.err = werr
		}
		progress <- p
		exited <- werr
	}()
	select {
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
		return Media{}, context.Cause(ctx)
	case <-exited:
	}
	p := <-progress
	if p.err != nil || p.frames != media.Frames {
		return Media{}, errors.New("Video conversion failed or produced incomplete frames.")
	}
	info, err := os.Stat(out)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 24 || info.Size() >= maxBytes {
		return Media{}, errors.New("Converted video exceeds its size limit or is empty.")
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return Media{}, err
	}
	if int64(len(data)) != info.Size() {
		return Media{}, errors.New("Converted video changed while being read.")
	}
	if string(data[4:8]) != "ftyp" {
		return Media{}, errors.New("Video converter returned an unexpected format.")
	}
	if err := ctx.Err(); err != nil {
		return Media{}, context.Cause(ctx)
	}
	return Media{Data: data, MimeType: "video/mp4", Format: "mp4", FPS: media.FPS, Frames: media.Frames}, nil
}

func writePrivate(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// readProgress reads ffmpeg's -progress output: the last frame=N, and
// progress=end, which must arrive.
func readProgress(r io.Reader) (int, error) {
	br := bufio.NewReader(r)
	frames, ended := -1, false
	for {
		line, err := br.ReadString('\n')
		if len(line) > diagnosticBytes {
			io.Copy(io.Discard, br)
			return 0, errors.New("Invalid video converter progress.")
		}
		if strings.HasSuffix(line, "\n") {
			l := strings.TrimSpace(line)
			if progressFrame.MatchString(l) {
				frames, _ = strconv.Atoi(l[6:])
			}
			if l == "progress=end" {
				ended = true
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if !ended {
		return 0, fmt.Errorf("Video converter did not finish.")
	}
	return frames, nil
}
