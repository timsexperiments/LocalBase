package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/video"
)

// newVideoJobs builds the video job manager over the video runtime (TS
// serve: VideoJobManager with the stable-diffusion.cpp client and the
// pinned converter). Jobs live under <root>/tmp/video-jobs.
func newVideoJobs(svc *runtime.Service, r *root.Root, log *slog.Logger) *video.Manager {
	if svc == nil {
		return nil
	}
	client, err := video.NewClient(svc.BaseURL() + "/")
	if err != nil {
		log.Warn("Video jobs are unavailable: "+err.Error(), "event", "video.unavailable", "category", "runtime", "runtime", "video")
		return nil
	}
	conv := &video.Converter{Ensure: func(ctx context.Context) (string, error) {
		if p := os.Getenv("LOCALBASE_FFMPEG"); p != "" {
			return filepath.Abs(p)
		}
		p, err := runtime.EnsureBinary(ctx, r.RuntimesDir(), "ffmpeg")
		if err != nil {
			return "", err
		}
		return filepath.Abs(p)
	}}
	jobs, err := video.NewManager(video.Options{Backend: client, TempDir: filepath.Join(r.Dir, "tmp"), Prepare: conv.Prepare,
		OnContainmentFailure: func(jobID, source string, err error) {
			log.Error("A local video job could not be contained.", "event", "video.job-containment-failed", "category", "runtime",
				"component", "video-job-manager", "runtime", "video", "error_type", "VideoContainmentFailure",
				"error", "A local video job could not be contained.")
		}})
	if err != nil {
		log.Warn("Video jobs are unavailable: "+err.Error(), "event", "video.unavailable", "category", "runtime", "runtime", "video")
		return nil
	}
	return jobs
}
