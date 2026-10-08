package mediaexec

// Command is a shell-free executable specification consumed by Runner.
type Command struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

func metadataCommand(config Config, dir, input string) Command {
	return command(config.FFprobePath, dir,
		"-hide_banner", "-v", "error", "-nostdin",
		"-protocol_whitelist", "file",
		"-show_entries", "format=format_name,duration:stream=codec_type,codec_name,width,height,duration,nb_frames",
		"-of", "json", input,
	)
}

func frameCommand(config Config, dir, input string) Command {
	return command(config.FFprobePath, dir,
		"-hide_banner", "-v", "error", "-nostdin",
		"-protocol_whitelist", "file",
		"-select_streams", "v:0", "-show_frames",
		"-show_entries", "frame=width,height",
		"-of", "json", input,
	)
}

func decodeCommand(config Config, dir, input string) Command {
	return command(config.FFmpegPath, dir,
		"-hide_banner", "-v", "error", "-xerror", "-nostdin",
		"-protocol_whitelist", "file", "-err_detect", "explode",
		"-i", input,
		"-map", "0:v?", "-map", "0:a?", "-sn", "-dn",
		"-f", "null", "-",
	)
}

func command(path, dir string, args ...string) Command {
	return Command{
		Path: path,
		Args: args,
		Env:  []string{"HOME=" + dir, "LANG=C", "LC_ALL=C", "TMPDIR=" + dir},
		Dir:  dir,
	}
}
