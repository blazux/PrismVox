// Package audio provides pure-Go WAV encoding/decoding and resampling utilities.
package audio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// EncodeWAV encodes float32 samples (values in [-1, 1]) to a standard 44-byte
// RIFF/WAVE PCM_16 mono file at the given sample rate.
func EncodeWAV(samples []float32, sampleRate int) ([]byte, error) {
	pcm := Float32ToInt16(samples)

	numSamples := len(pcm)
	dataSize := numSamples * 2
	fileSize := 36 + dataSize

	buf := new(bytes.Buffer)
	buf.Grow(44 + dataSize)

	// RIFF header
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(fileSize)) //nolint:errcheck
	buf.WriteString("WAVE")

	// fmt chunk
	buf.WriteString("fmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16))           // chunk size //nolint:errcheck
	binary.Write(buf, binary.LittleEndian, uint16(1))            // PCM //nolint:errcheck
	binary.Write(buf, binary.LittleEndian, uint16(1))            // mono //nolint:errcheck
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate))   // sample rate //nolint:errcheck
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate*2)) // byte rate //nolint:errcheck
	binary.Write(buf, binary.LittleEndian, uint16(2))            // block align //nolint:errcheck
	binary.Write(buf, binary.LittleEndian, uint16(16))           // bits per sample //nolint:errcheck

	// data chunk
	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(dataSize)) //nolint:errcheck

	// PCM samples (little-endian)
	for _, s := range pcm {
		binary.Write(buf, binary.LittleEndian, s) //nolint:errcheck
	}

	return buf.Bytes(), nil
}

// DecodeWAV decodes a WAV file and returns float32 samples and sample rate.
// Supports 16-bit PCM mono/stereo (downmixes to mono).
func DecodeWAV(data []byte) ([]float32, int, error) {
	r := bytes.NewReader(data)

	// Read RIFF header
	var riff [4]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return nil, 0, fmt.Errorf("wav: read RIFF: %w", err)
	}
	if string(riff[:]) != "RIFF" {
		return nil, 0, fmt.Errorf("wav: not a RIFF file")
	}
	var chunkSize uint32
	binary.Read(r, binary.LittleEndian, &chunkSize) //nolint:errcheck
	var wave [4]byte
	if _, err := io.ReadFull(r, wave[:]); err != nil {
		return nil, 0, fmt.Errorf("wav: read WAVE: %w", err)
	}
	if string(wave[:]) != "WAVE" {
		return nil, 0, fmt.Errorf("wav: not a WAVE file")
	}

	var (
		sampleRate  int
		numChannels int
		bitsPerSamp int
		pcmData     []byte
	)

	// Read chunks
	for {
		var id [4]byte
		if _, err := io.ReadFull(r, id[:]); err != nil {
			break
		}
		var size uint32
		if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
			break
		}
		switch string(id[:]) {
		case "fmt ":
			var audioFmt uint16
			binary.Read(r, binary.LittleEndian, &audioFmt) //nolint:errcheck
			var ch uint16
			binary.Read(r, binary.LittleEndian, &ch) //nolint:errcheck
			numChannels = int(ch)
			var sr uint32
			binary.Read(r, binary.LittleEndian, &sr) //nolint:errcheck
			sampleRate = int(sr)
			var byteRate uint32
			binary.Read(r, binary.LittleEndian, &byteRate) //nolint:errcheck
			var blockAlign uint16
			binary.Read(r, binary.LittleEndian, &blockAlign) //nolint:errcheck
			var bps uint16
			binary.Read(r, binary.LittleEndian, &bps) //nolint:errcheck
			bitsPerSamp = int(bps)
			// skip any extra fmt bytes
			remaining := int(size) - 16
			if remaining > 0 {
				io.CopyN(io.Discard, r, int64(remaining)) //nolint:errcheck
			}
		case "data":
			pcmData = make([]byte, size)
			io.ReadFull(r, pcmData) //nolint:errcheck
		default:
			io.CopyN(io.Discard, r, int64(size)) //nolint:errcheck
		}
	}

	if bitsPerSamp != 16 {
		return nil, 0, fmt.Errorf("wav: unsupported bits per sample: %d (only 16 supported)", bitsPerSamp)
	}
	if numChannels < 1 {
		return nil, 0, fmt.Errorf("wav: invalid channel count")
	}

	// Parse int16 samples
	numFrames := len(pcmData) / (2 * numChannels)
	out := make([]float32, numFrames)
	br := bytes.NewReader(pcmData)
	for i := 0; i < numFrames; i++ {
		var sum int32
		for ch := 0; ch < numChannels; ch++ {
			var s int16
			binary.Read(br, binary.LittleEndian, &s) //nolint:errcheck
			sum += int32(s)
		}
		out[i] = float32(sum/int32(numChannels)) / 32768.0
	}

	return out, sampleRate, nil
}

// resampleHalfWidth is the windowed-sinc kernel half-width in input samples
// (2*N+1 taps). Wider = sharper anti-aliasing, more compute.
const resampleHalfWidth = 24

// Resample converts samples from fromRate to toRate with a windowed-sinc (Lanczos)
// kernel. Crucially, when downsampling it band-limits to the output Nyquist, so
// content above it does not alias back as audible noise — the naive linear
// interpolation it replaces let 24kHz→8kHz fold high frequencies into the phone
// band as artifacts.
func Resample(samples []float32, fromRate, toRate int) []float32 {
	if fromRate == toRate || len(samples) == 0 {
		return samples
	}
	ratio := float64(fromRate) / float64(toRate) // input samples per output sample
	outLen := int(math.Ceil(float64(len(samples)) / ratio))
	out := make([]float32, outLen)

	// Cutoff in cycles per input sample: the output Nyquist when downsampling,
	// otherwise the input Nyquist (pure interpolation, no band-limiting needed).
	cutoff := 0.5
	if toRate < fromRate {
		cutoff = 0.5 * float64(toRate) / float64(fromRate)
	}

	for i := range out {
		center := float64(i) * ratio
		c := int(math.Floor(center))
		var acc, wsum float64
		for j := c - resampleHalfWidth; j <= c+resampleHalfWidth; j++ {
			if j < 0 || j >= len(samples) {
				continue
			}
			x := center - float64(j) // distance in input samples
			k := 2 * cutoff * sinc(2*cutoff*x) * hann(x, resampleHalfWidth)
			acc += float64(samples[j]) * k
			wsum += k
		}
		if wsum != 0 {
			out[i] = float32(acc / wsum)
		}
	}
	return out
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

// hann is a Hann window over [-half, half], zero outside.
func hann(x, half float64) float64 {
	if x < -half || x > half {
		return 0
	}
	return 0.5 + 0.5*math.Cos(math.Pi*x/half)
}

// Int16ToFloat32 converts int16 samples to float32 in [-1, 1].
func Int16ToFloat32(samples []int16) []float32 {
	out := make([]float32, len(samples))
	for i, s := range samples {
		out[i] = float32(s) / 32768.0
	}
	return out
}

// Float32ToInt16 converts float32 samples in [-1, 1] to int16, clamping.
func Float32ToInt16(samples []float32) []int16 {
	out := make([]int16, len(samples))
	for i, s := range samples {
		if s > 1.0 {
			s = 1.0
		} else if s < -1.0 {
			s = -1.0
		}
		out[i] = int16(s * 32767)
	}
	return out
}
