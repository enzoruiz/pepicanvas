package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/mediaexec"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

const (
	defaultFFprobePath = "/usr/bin/ffprobe"
	defaultFFmpegPath  = "/usr/bin/ffmpeg"
)

// mediaVariables enable media storage when any of them is set, and each is
// then required.
var mediaVariables = []string{
	"PEPICANVAS_S3_ENDPOINT",
	"PEPICANVAS_S3_BUCKET",
	"PEPICANVAS_S3_ACCESS_KEY",
	"PEPICANVAS_S3_SECRET_KEY",
	"PEPICANVAS_MEDIA_CGROUP_ROOT",
}

// newMediaRunner builds the containment of every inspection. Tests replace
// it, because the cgroup runner needs a delegated cgroup-v2 subtree.
var newMediaRunner = mediaexec.NewCgroupRunner

// mediaConfig is the object storage and inspection configuration.
type mediaConfig struct {
	storage    s3.Config
	cgroupRoot string
	inspection mediaexec.Config
}

// mediaFromEnv reads the media configuration. It returns nil when no media
// variable is set, and an error, which names variables and never their
// values, when the configuration is partial or invalid.
func mediaFromEnv(getenv func(string) string) (*mediaConfig, error) {
	var missing []string
	for _, name := range mediaVariables {
		if getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == len(mediaVariables) && getenv("PEPICANVAS_S3_SECURE") == "" {
		return nil, nil
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("media storage is partially configured: missing %s", strings.Join(missing, ", "))
	}
	secure := true
	switch getenv("PEPICANVAS_S3_SECURE") {
	case "", "true":
	case "false":
		secure = false
	default:
		return nil, errors.New("PEPICANVAS_S3_SECURE must be true or false")
	}
	return &mediaConfig{
		storage: s3.Config{
			Endpoint:  getenv("PEPICANVAS_S3_ENDPOINT"),
			Bucket:    getenv("PEPICANVAS_S3_BUCKET"),
			AccessKey: secret.New(secret.KindPassword, getenv("PEPICANVAS_S3_ACCESS_KEY")),
			SecretKey: secret.New(secret.KindPassword, getenv("PEPICANVAS_S3_SECRET_KEY")),
			Secure:    secure,
		},
		cgroupRoot: getenv("PEPICANVAS_MEDIA_CGROUP_ROOT"),
		inspection: mediaexec.Config{
			FFprobePath: valueOr(getenv("PEPICANVAS_FFPROBE_PATH"), defaultFFprobePath),
			FFmpegPath:  valueOr(getenv("PEPICANVAS_FFMPEG_PATH"), defaultFFmpegPath),
			TempRoot:    valueOr(getenv("PEPICANVAS_MEDIA_TEMP_DIR"), os.TempDir()),
			Limits:      media.DefaultLimits(),
		},
	}, nil
}

// inspector builds the contained ffprobe/ffmpeg inspector.
func (c *mediaConfig) inspector() (media.Inspector, error) {
	runner, result := newMediaRunner(c.cgroupRoot)
	if runner == nil || result != mediaexec.SuccessfulResult() {
		return nil, errors.New("media inspection containment is unavailable: PEPICANVAS_MEDIA_CGROUP_ROOT must be a delegated cgroup v2 subtree")
	}
	adapter, err := mediaexec.New(c.inspection, runner)
	if err != nil {
		return nil, fmt.Errorf("media inspector: %w", err)
	}
	return adapter, nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
