package session

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

const zstdMagic uint32 = 0xFD2FB528

// frameRange is one structurally complete Zstandard frame.
type frameRange struct {
	start, end int
}

// zstdFrames locates complete concatenated frames. A torn final frame returns
// tornStart >= 0 with only the complete prefix.
func zstdFrames(buf []byte) (frames []frameRange, tornStart int, err error) {
	offset := 0
	for offset < len(buf) {
		start := offset
		if len(buf)-offset < 4 {
			return frames, start, nil
		}
		magic := uint32(buf[offset]) | uint32(buf[offset+1])<<8 | uint32(buf[offset+2])<<16 | uint32(buf[offset+3])<<24
		if magic != zstdMagic {
			return nil, 0, fmt.Errorf("corrupt Zstandard session log: invalid frame magic at byte %d", offset)
		}
		offset += 4
		if offset == len(buf) {
			return frames, start, nil
		}
		descriptor := buf[offset]
		offset++
		if descriptor&0x18 != 0 {
			return nil, 0, fmt.Errorf("corrupt Zstandard session log: reserved frame-header bit at byte %d", offset-1)
		}
		contentSizeFlag := descriptor >> 6
		singleSegment := descriptor&0x20 != 0
		checksum := descriptor&0x04 != 0
		dictionaryFlag := descriptor & 0x03
		dictionaryBytes := int(dictionaryFlag)
		if dictionaryFlag == 3 {
			dictionaryBytes = 4
		}
		contentSizeBytes := 0
		if contentSizeFlag == 0 {
			if singleSegment {
				contentSizeBytes = 1
			}
		} else {
			contentSizeBytes = 1 << contentSizeFlag
		}
		remainingHeader := dictionaryBytes + contentSizeBytes
		if !singleSegment {
			remainingHeader++
		}
		if len(buf)-offset < remainingHeader {
			return frames, start, nil
		}
		offset += remainingHeader
		for {
			if len(buf)-offset < 3 {
				return frames, start, nil
			}
			blockHeader := uint32(buf[offset]) | uint32(buf[offset+1])<<8 | uint32(buf[offset+2])<<16
			offset += 3
			lastBlock := blockHeader&1 != 0
			blockType := (blockHeader >> 1) & 0x03
			blockSize := int(blockHeader >> 3)
			if blockType == 0x03 {
				return nil, 0, fmt.Errorf("corrupt Zstandard session log: reserved block type at byte %d", offset-3)
			}
			payloadBytes := blockSize
			if blockType == 0x01 {
				payloadBytes = 1
			}
			if len(buf)-offset < payloadBytes {
				return frames, start, nil
			}
			offset += payloadBytes
			if lastBlock {
				break
			}
		}
		if checksum {
			if len(buf)-offset < 4 {
				return frames, start, nil
			}
			offset += 4
		}
		frames = append(frames, frameRange{start: start, end: offset})
	}
	return frames, -1, nil
}

// frameEncoder writes one independently decodable, checksummed frame per call,
// matching the DeepSeek Harness container framing.
type frameEncoder struct {
	enc *zstd.Encoder
}

func newFrameEncoder() (*frameEncoder, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(true),
		zstd.WithZeroFrames(true),
		zstd.WithSingleSegment(false),
	)
	if err != nil {
		return nil, fmt.Errorf("session: zstd encoder: %w", err)
	}
	return &frameEncoder{enc: enc}, nil
}

// compressFrame encodes one complete frame.
func (e *frameEncoder) compressFrame(plain []byte) []byte {
	return e.enc.EncodeAll(plain, nil)
}

// release frees encoder resources.
func (e *frameEncoder) release() {
	if e != nil && e.enc != nil {
		e.enc.Close()
	}
}

// frameDecoder decodes complete frames in source order.
type frameDecoder struct {
	dec *zstd.Decoder
}

func newFrameDecoder() (*frameDecoder, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("session: zstd decoder: %w", err)
	}
	return &frameDecoder{dec: dec}, nil
}

// decompressFrame decodes one complete frame and validates its checksum.
func (d *frameDecoder) decompressFrame(frame []byte) ([]byte, error) {
	out, err := d.dec.DecodeAll(frame, nil)
	if err != nil {
		return nil, fmt.Errorf("session: corrupt Zstandard frame: %w", err)
	}
	return out, nil
}

// release frees decoder resources.
func (d *frameDecoder) release() {
	if d != nil && d.dec != nil {
		d.dec.Close()
	}
}

// errTornTail reports a final plaintext fragment without its newline, or a
// final Zstandard frame that never completed. Such a fragment belongs to an
// append that never resolved and is not part of the log.
var errTornTail = errors.New("session: torn tail")

// decodeLog reads a whole artifact, returning the committed plaintext prefix.
// A structurally complete log decodes byte-for-byte; a torn tail is dropped.
func decodeLog(raw []byte, compression Compression) ([]byte, error) {
	if compression == CompressionNone {
		committed := bytes.LastIndexByte(raw, '\n')
		if committed < 0 {
			return nil, errTornTail
		}
		return raw[:committed+1], nil
	}
	frames, torn, err := zstdFrames(raw)
	if err != nil {
		return nil, err
	}
	_ = torn
	decoder, err := newFrameDecoder()
	if err != nil {
		return nil, err
	}
	defer decoder.release()
	var out bytes.Buffer
	for _, frame := range frames {
		plain, err := decoder.decompressFrame(raw[frame.start:frame.end])
		if err != nil {
			return nil, err
		}
		out.Write(plain)
	}
	return out.Bytes(), nil
}

// encodeFrames frames a plaintext artifact so its first frame is exactly the
// header line and each later frame is one append batch.
func encodeFrames(header []byte, batches [][]byte) ([]byte, error) {
	encoder, err := newFrameEncoder()
	if err != nil {
		return nil, err
	}
	defer encoder.release()
	var out bytes.Buffer
	out.Write(encoder.compressFrame(header))
	for _, batch := range batches {
		if len(batch) == 0 {
			continue
		}
		out.Write(encoder.compressFrame(batch))
	}
	return out.Bytes(), nil
}
