package mediaexec

import (
	"bufio"
	"encoding/binary"
	"io"
)

const (
	gifExtensionIntroducer = 0x21
	gifImageSeparator      = 0x2C
	gifTrailer             = 0x3B
	gifColorTableFlag      = 0x80
)

// gifFramesFitScreen walks the blocks of a GIF without decoding its image data
// and requires every image descriptor to declare a non-empty frame inside the
// logical screen. ffmpeg clips a frame that overflows the screen and only
// warns, so the decoded frames alone cannot reveal the contradiction.
func gifFramesFitScreen(source io.Reader) error {
	reader := bufio.NewReader(source)
	var header [13]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return newError(ErrorInvalidFrames)
	}
	if signature := string(header[:6]); signature != "GIF87a" && signature != "GIF89a" {
		return newError(ErrorInvalidFrames)
	}
	screenWidth := int(binary.LittleEndian.Uint16(header[6:8]))
	screenHeight := int(binary.LittleEndian.Uint16(header[8:10]))
	if !skipColorTable(reader, header[10]) {
		return newError(ErrorInvalidFrames)
	}

	frames := 0
	for {
		block, err := reader.ReadByte()
		if err != nil {
			return newError(ErrorInvalidFrames)
		}
		switch block {
		case gifExtensionIntroducer:
			if _, err := reader.ReadByte(); err != nil || !skipSubBlocks(reader) {
				return newError(ErrorInvalidFrames)
			}
		case gifImageSeparator:
			var descriptor [9]byte
			if _, err := io.ReadFull(reader, descriptor[:]); err != nil {
				return newError(ErrorInvalidFrames)
			}
			left := int(binary.LittleEndian.Uint16(descriptor[0:2]))
			top := int(binary.LittleEndian.Uint16(descriptor[2:4]))
			width := int(binary.LittleEndian.Uint16(descriptor[4:6]))
			height := int(binary.LittleEndian.Uint16(descriptor[6:8]))
			if width == 0 || height == 0 || left+width > screenWidth || top+height > screenHeight {
				return newError(ErrorInvalidFrames)
			}
			// The LZW minimum code size precedes the image data sub-blocks.
			if !skipColorTable(reader, descriptor[8]) || !discard(reader, 1) || !skipSubBlocks(reader) {
				return newError(ErrorInvalidFrames)
			}
			frames++
		case gifTrailer:
			if frames == 0 {
				return newError(ErrorInvalidFrames)
			}
			return nil
		default:
			return newError(ErrorInvalidFrames)
		}
	}
}

func skipColorTable(reader *bufio.Reader, packed byte) bool {
	if packed&gifColorTableFlag == 0 {
		return true
	}
	return discard(reader, 3<<((packed&0x07)+1))
}

func skipSubBlocks(reader *bufio.Reader) bool {
	for {
		size, err := reader.ReadByte()
		if err != nil {
			return false
		}
		if size == 0 {
			return true
		}
		if !discard(reader, int(size)) {
			return false
		}
	}
}

func discard(reader *bufio.Reader, count int) bool {
	discarded, err := reader.Discard(count)
	return err == nil && discarded == count
}
