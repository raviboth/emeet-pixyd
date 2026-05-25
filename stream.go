//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"time"
)

func (s *webServer) handleSnapshot(responseWriter http.ResponseWriter, _ *http.Request) {
	s.daemon.lastFrame.RLock()
	frame := s.daemon.lastFrame.data
	s.daemon.lastFrame.RUnlock()

	if len(frame) == 0 {
		http.Error(responseWriter, "no frame available", http.StatusServiceUnavailable)

		return
	}

	responseWriter.Header().Set("Content-Type", "image/jpeg")
	responseWriter.Header().Set("Cache-Control", "no-store")
	_, _ = responseWriter.Write(frame)
}

func ffmpegStreamCmd(ctx context.Context, device string) *exec.Cmd {
	// Input flags below trim ffmpeg's default frame queueing so PTZ changes
	// surface in the preview within ~1 frame instead of buffering for a
	// second or more. -fflags nobuffer disables the input frame queue;
	// -flags low_delay disables decoder reordering; -probesize / -analyzeduration
	// skip the long codec sniff at stream start (we already know it is mjpeg);
	// -fpsprobesize 0 skips the framerate probe (UVC reports it directly).
	return exec.CommandContext(ctx,
		"ffmpeg",
		"-hide_banner",
		"-loglevel", "warning",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-probesize", "32",
		"-analyzeduration", "0",
		"-fpsprobesize", "0",
		"-f", "v4l2",
		"-input_format", "mjpeg",
		"-video_size", "1920x1080",
		"-framerate", "30",
		"-thread_queue_size", "32",
		"-i", device,
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-q:v", "5",
		"-vf", "scale=640:-1",
		"pipe:1",
	)
}

// cleanupFFmpeg terminates the ffmpeg child after a stream request ends.
// SIGKILL is used directly rather than SIGTERM-then-wait: MJPEG-over-pipe
// has no per-frame state worth flushing, and the daemon's stream
// semaphore is held until this returns, so any grace period directly
// translates into a "stream already in use" 503 window for the client's
// reconnect-after-PTZ flow. ffmpegShutdown bounds how long Wait may
// block after SIGKILL (it almost always returns within a few ms).
func cleanupFFmpeg(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(ffmpegShutdown):
		// Process did not get reaped; orphan the wait goroutine and
		// move on so we do not hold the stream semaphore forever.
	}
}

func (s *webServer) handleStream(
	responseWriter http.ResponseWriter,
	request *http.Request,
) {
	// Wait briefly for the stream slot. This covers the common case of a
	// reconnect (e.g. PTZ-induced reload, page refresh, htmx swap) racing
	// the previous request's ffmpeg cleanup. Without the wait the new
	// request lands on 503 within microseconds, the browser fires
	// img.onerror, and the user sees a black screen until the exponential
	// backoff retry fires seconds later. Beyond the timeout we still
	// return 503 so a genuinely-stuck slot does not block forever.
	const streamSemaWait = time.Second
	semaCtx, semaCancel := context.WithTimeout(request.Context(), streamSemaWait)
	select {
	case s.daemon.streamSema <- struct{}{}:
		semaCancel()
	case <-semaCtx.Done():
		semaCancel()
		http.Error(responseWriter, "stream already in use", http.StatusServiceUnavailable)
		return
	}
	defer func() { <-s.daemon.streamSema }()

	status, ok := s.checkDevice(responseWriter)
	if !ok {
		return
	}
	if _, lookErr := exec.LookPath("ffmpeg"); lookErr != nil {
		http.Error(responseWriter, "ffmpeg not available", http.StatusServiceUnavailable)

		return
	}
	flusher, flushOk := responseWriter.(http.Flusher)
	if !flushOk {

		http.Error(responseWriter, "streaming not supported", http.StatusInternalServerError)

		return
	}
	rc := http.NewResponseController(responseWriter)
	// Per-frame write deadline below. Do not clear the deadline up front;
	// each iteration of the frame loop refreshes it so a slow / stuck
	// client (e.g. tab moved to a background workspace, suspended laptop,
	// network stall) does not pin the daemon's single stream slot forever
	// via TCP back-pressure. The deadline is generous so a transient
	// pause in the client does not drop the connection.
	ctx, cancel := context.WithCancel(request.Context())
	s.daemon.streamMu.Lock()
	s.daemon.streamCancel = cancel
	s.daemon.streamMu.Unlock()
	defer func() {
		s.daemon.streamMu.Lock()
		if s.daemon.streamCancel != nil {
			s.daemon.streamCancel = nil
		}
		s.daemon.streamMu.Unlock()
		cancel()
	}()
	cmd := ffmpegStreamCmd(ctx, status.Device)
	stdOut, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		http.Error(responseWriter, "stream pipe error", http.StatusInternalServerError)

		return
	}
	stdErr, _ := cmd.StderrPipe()
	if stdErr != nil {
		go func() {
			scanner := bufio.NewScanner(stdErr)
			for scanner.Scan() {
				slog.Debug("ffmpeg", "line", scanner.Text())
			}
		}()
	}
	startErr := cmd.Start()
	if startErr != nil {
		http.Error(responseWriter, "stream start error", http.StatusInternalServerError)

		return
	}
	responseWriter.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	responseWriter.Header().Set("Cache-Control", "no-store")
	defer cleanupFFmpeg(cmd)
	br := bufio.NewReaderSize(stdOut, streamBufSize)
	var buf bytes.Buffer
	for {

		select {

		case <-ctx.Done():

			return

		default:

		}

		frame, frameErr := extractJPEGFrame(br, &buf)

		if frameErr != nil {

			slog.Debug("frame extract error", "error", frameErr)

			return

		}

		s.daemon.lastFrame.Lock()
		s.daemon.lastFrame.data = frame
		s.daemon.lastFrame.Unlock()

		// Reset the write deadline for this frame. If the client is not
		// draining, the three writes below return os.ErrDeadlineExceeded
		// and the handler unwinds, releasing the stream semaphore so the
		// next request (e.g. the user's panel reload) can take over.
		if dlErr := rc.SetWriteDeadline(time.Now().Add(streamFrameWriteTimeout)); dlErr != nil {
			slog.Debug("set write deadline", "error", dlErr)
		}

		_, headerErr := fmt.Fprintf(

			responseWriter,

			"--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n",

			len(frame),
		)

		if headerErr != nil {
			return
		}

		_, writeErr := responseWriter.Write(frame)

		if writeErr != nil {
			return
		}

		_, sepErr := fmt.Fprint(responseWriter, "\r\n")

		if sepErr != nil {
			return
		}

		flusher.Flush()
	}
}

func extractJPEGFrame(br *bufio.Reader, buf *bytes.Buffer) ([]byte, error) {
	var soiFound bool
	for {
		if buf.Len() > maxStreamBufferSize {
			buf.Reset()
		}

		b, err := br.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read byte: %w", err)
		}

		if !soiFound {
			if b == 0xFF {
				next, nextErr := br.ReadByte()
				if nextErr != nil {
					return nil, fmt.Errorf("read soi next: %w", nextErr)
				}
				switch next {
				case 0xD8:
					buf.Reset()
					buf.Write([]byte{0xFF, 0xD8})
					soiFound = true
				case 0xFF:
					_ = br.UnreadByte()
				}
			}
			continue
		}

		buf.WriteByte(b)

		if b == 0xFF {
			next, nextErr := br.ReadByte()
			if nextErr != nil {
				return nil, fmt.Errorf("read eoi next: %w", nextErr)
			}
			buf.WriteByte(next)
			if next == 0xD9 {
				frame := make([]byte, buf.Len())
				copy(frame, buf.Bytes())
				return frame, nil
			}
		}
	}
}
