package mediaexec

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCommandSpecsUseExactShellFreeTokensAndPrivateEnvironment(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(string(filepath.Separator), "private", "job-1")
	input := filepath.Join(dir, "input")
	config := Config{FFprobePath: "/tools/ffprobe", FFmpegPath: "/tools/ffmpeg"}
	tests := []struct {
		name string
		got  Command
		path string
		args []string
	}{
		{
			name: "metadata probe",
			got:  metadataCommand(config, dir, input),
			path: config.FFprobePath,
			args: []string{"-hide_banner", "-v", "error", "-nostdin", "-protocol_whitelist", "file", "-show_entries", "format=format_name,duration:stream=codec_type,codec_name,width,height,duration,nb_frames", "-of", "json", input},
		},
		{
			name: "GIF frame probe",
			got:  frameCommand(config, dir, input),
			path: config.FFprobePath,
			args: []string{"-hide_banner", "-v", "error", "-nostdin", "-protocol_whitelist", "file", "-select_streams", "v:0", "-show_frames", "-show_entries", "frame=width,height", "-of", "json", input},
		},
		{
			name: "complete decode",
			got:  decodeCommand(config, dir, input),
			path: config.FFmpegPath,
			args: []string{"-hide_banner", "-v", "error", "-xerror", "-nostdin", "-protocol_whitelist", "file", "-err_detect", "explode", "-i", input, "-map", "0:v?", "-map", "0:a?", "-sn", "-dn", "-f", "null", "-"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got.Path != tt.path || !reflect.DeepEqual(tt.got.Args, tt.args) {
				t.Fatalf("command = %#v, want path %q and args %#v", tt.got, tt.path, tt.args)
			}
			wantEnv := []string{"HOME=" + dir, "LANG=C", "LC_ALL=C", "TMPDIR=" + dir}
			if !reflect.DeepEqual(tt.got.Env, wantEnv) || tt.got.Dir != dir {
				t.Fatalf("environment/dir = %#v/%q, want %#v/%q", tt.got.Env, tt.got.Dir, wantEnv, dir)
			}
			for _, argument := range tt.got.Args {
				if argument == "sh" || argument == "-c" || strings.ContainsAny(argument, ";|$`") {
					t.Fatalf("arguments %#v contain shell/interpolation token %q", tt.got.Args, argument)
				}
			}
		})
	}
}
