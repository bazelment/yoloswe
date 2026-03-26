package stt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"
)

// EncodeWAV writes a WAV file with 16-bit mono PCM samples at the given sample rate.
func EncodeWAV(w io.Writer, samples []int16, sampleRate int) error {
	dataSize := len(samples) * 2
	fileSize := 36 + dataSize

	// RIFF header
	if _, err := w.Write([]byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(fileSize)); err != nil {
		return err
	}
	if _, err := w.Write([]byte("WAVE")); err != nil {
		return err
	}

	// fmt chunk
	if _, err := w.Write([]byte("fmt ")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(16)); err != nil { // chunk size
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(1)); err != nil { // PCM format
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(1)); err != nil { // mono
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(sampleRate)); err != nil {
		return err
	}
	byteRate := sampleRate * 2 // 16-bit mono
	if err := binary.Write(w, binary.LittleEndian, uint32(byteRate)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(2)); err != nil { // block align
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(16)); err != nil { // bits per sample
		return err
	}

	// data chunk
	if _, err := w.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(dataSize)); err != nil {
		return err
	}
	return binary.Write(w, binary.LittleEndian, samples)
}

// DecodeWAV reads a WAV file and returns 16-bit mono PCM samples and the sample rate.
// It returns an error if the file is not a valid WAV or is not 16-bit mono PCM.
func DecodeWAV(r io.Reader) (samples []int16, sampleRate int, err error) {
	var buf [4]byte

	// RIFF header
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, 0, fmt.Errorf("read RIFF: %w", err)
	}
	if string(buf[:]) != "RIFF" {
		return nil, 0, fmt.Errorf("not a RIFF file: got %q", string(buf[:]))
	}

	var fileSize uint32
	if err := binary.Read(r, binary.LittleEndian, &fileSize); err != nil {
		return nil, 0, fmt.Errorf("read file size: %w", err)
	}

	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, 0, fmt.Errorf("read WAVE: %w", err)
	}
	if string(buf[:]) != "WAVE" {
		return nil, 0, fmt.Errorf("not a WAVE file: got %q", string(buf[:]))
	}

	// Read chunks until we find "data"
	var numChannels uint16
	var bitsPerSample uint16
	var sr uint32
	foundFmt := false

	for {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			if err == io.EOF {
				return nil, 0, fmt.Errorf("unexpected EOF: no data chunk found")
			}
			return nil, 0, fmt.Errorf("read chunk id: %w", err)
		}
		chunkID := string(buf[:])

		var chunkSize uint32
		if err := binary.Read(r, binary.LittleEndian, &chunkSize); err != nil {
			return nil, 0, fmt.Errorf("read chunk size: %w", err)
		}

		switch chunkID {
		case "fmt ":
			var audioFormat uint16
			if err := binary.Read(r, binary.LittleEndian, &audioFormat); err != nil {
				return nil, 0, fmt.Errorf("read audio format: %w", err)
			}
			if audioFormat != 1 {
				return nil, 0, fmt.Errorf("unsupported audio format %d (only PCM=1)", audioFormat)
			}
			if err := binary.Read(r, binary.LittleEndian, &numChannels); err != nil {
				return nil, 0, fmt.Errorf("read channels: %w", err)
			}
			if numChannels != 1 {
				return nil, 0, fmt.Errorf("unsupported channel count %d (only mono=1)", numChannels)
			}
			if err := binary.Read(r, binary.LittleEndian, &sr); err != nil {
				return nil, 0, fmt.Errorf("read sample rate: %w", err)
			}
			// Skip byte rate and block align
			var byteRate uint32
			var blockAlign uint16
			if err := binary.Read(r, binary.LittleEndian, &byteRate); err != nil {
				return nil, 0, fmt.Errorf("read byte rate: %w", err)
			}
			if err := binary.Read(r, binary.LittleEndian, &blockAlign); err != nil {
				return nil, 0, fmt.Errorf("read block align: %w", err)
			}
			if err := binary.Read(r, binary.LittleEndian, &bitsPerSample); err != nil {
				return nil, 0, fmt.Errorf("read bits per sample: %w", err)
			}
			if bitsPerSample != 16 {
				return nil, 0, fmt.Errorf("unsupported bits per sample %d (only 16)", bitsPerSample)
			}
			foundFmt = true
			// Skip any extra fmt bytes
			remaining := int(chunkSize) - 16
			if remaining > 0 {
				if _, err := io.CopyN(io.Discard, r, int64(remaining)); err != nil {
					return nil, 0, fmt.Errorf("skip extra fmt bytes: %w", err)
				}
			}

		case "data":
			if !foundFmt {
				return nil, 0, fmt.Errorf("data chunk before fmt chunk")
			}
			numSamples := int(chunkSize) / 2
			samples = make([]int16, numSamples)
			if err := binary.Read(r, binary.LittleEndian, samples); err != nil {
				return nil, 0, fmt.Errorf("read samples: %w", err)
			}
			return samples, int(sr), nil

		default:
			// Skip unknown chunks
			if _, err := io.CopyN(io.Discard, r, int64(chunkSize)); err != nil {
				return nil, 0, fmt.Errorf("skip chunk %q: %w", chunkID, err)
			}
		}
	}
}

// GenerateSineWAV creates a WAV file in memory containing a sine wave tone.
func GenerateSineWAV(freq float64, duration time.Duration, sampleRate int) []byte {
	numSamples := int(duration.Seconds() * float64(sampleRate))
	samples := make([]int16, numSamples)
	for i := range samples {
		t := float64(i) / float64(sampleRate)
		samples[i] = int16(math.Sin(2*math.Pi*freq*t) * 0.8 * math.MaxInt16)
	}
	var buf bytes.Buffer
	if err := EncodeWAV(&buf, samples, sampleRate); err != nil {
		panic("GenerateSineWAV: " + err.Error())
	}
	return buf.Bytes()
}

// GenerateSilenceWAV creates a WAV file in memory containing silence.
func GenerateSilenceWAV(duration time.Duration, sampleRate int) []byte {
	numSamples := int(duration.Seconds() * float64(sampleRate))
	samples := make([]int16, numSamples)
	var buf bytes.Buffer
	if err := EncodeWAV(&buf, samples, sampleRate); err != nil {
		panic("GenerateSilenceWAV: " + err.Error())
	}
	return buf.Bytes()
}

// ChunkPCM splits raw PCM data into fixed-size chunks.
// The last chunk may be smaller than chunkSize if data is not evenly divisible.
func ChunkPCM(data []byte, chunkSize int) [][]byte {
	if chunkSize <= 0 {
		return nil
	}
	var chunks [][]byte
	for len(data) > 0 {
		end := chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunks = append(chunks, data[:end])
		data = data[end:]
	}
	return chunks
}

// PCMFromWAV extracts raw PCM data (without the WAV header) from WAV bytes.
func PCMFromWAV(wavData []byte) ([]byte, error) {
	samples, _, err := DecodeWAV(bytes.NewReader(wavData))
	if err != nil {
		return nil, err
	}
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return buf, nil
}
