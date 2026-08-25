package whatsapp

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"testing"
)

func requireAudioTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(mediaExecutable(name)); err != nil {
			t.Skipf("%s is required for voice-note conversion tests", name)
		}
	}
}

func testWAV(sampleRate, seconds, amplitude int) []byte {
	sampleCount := sampleRate * seconds
	dataSize := sampleCount * 2
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(36+dataSize))
	wav.WriteString("WAVEfmt ")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(16))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(1))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(1))
	_ = binary.Write(&wav, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&wav, binary.LittleEndian, uint32(sampleRate*2))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(2))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(16))
	wav.WriteString("data")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(dataSize))
	for i := range sampleCount {
		sample := int16((i%100)-50) * int16(amplitude)
		_ = binary.Write(&wav, binary.LittleEndian, sample)
	}
	return wav.Bytes()
}

func peakPCMAmplitude(t *testing.T, audioData []byte) int {
	t.Helper()
	audioPath := t.TempDir() + "/voice.ogg"
	if err := os.WriteFile(audioPath, audioData, 0600); err != nil {
		t.Fatalf("failed to write converted audio: %v", err)
	}
	pcm, err := exec.Command(
		mediaExecutable("ffmpeg"),
		"-v", "error",
		"-i", audioPath,
		"-f", "s16le",
		"-ac", "1",
		"-ar", "16000",
		"pipe:1",
	).Output()
	if err != nil {
		t.Fatalf("failed to decode converted audio: %v", err)
	}

	peak := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := int(int16(binary.LittleEndian.Uint16(pcm[i : i+2])))
		if sample < 0 {
			sample = -sample
		}
		if sample > peak {
			peak = sample
		}
	}
	return peak
}

func TestConvertAudioToVoiceNote(t *testing.T) {
	requireAudioTools(t)

	voiceData, duration, waveformData, err := convertAudioToVoiceNote(context.Background(), testWAV(8000, 1, 2))
	if err != nil {
		t.Fatalf("convertAudioToVoiceNote returned an error: %v", err)
	}
	if !bytes.HasPrefix(voiceData, []byte("OggS")) {
		t.Fatalf("converted audio is not an Ogg stream: header %q", voiceData[:4])
	}
	if duration != 1 {
		t.Fatalf("duration = %d, want 1", duration)
	}
	if len(waveformData) != 64 {
		t.Fatalf("waveform length = %d, want 64", len(waveformData))
	}
	hasSignal := false
	for _, sample := range waveformData {
		if sample > 0 {
			hasSignal = true
			break
		}
	}
	if !hasSignal {
		t.Fatal("waveform contains no audible signal")
	}
	if peak := peakPCMAmplitude(t, voiceData); peak < 3000 {
		t.Fatalf("converted audio peak = %d, want at least 3000", peak)
	}
}

func TestConvertAudioToVoiceNoteRejectsInvalidAudio(t *testing.T) {
	requireAudioTools(t)

	if _, _, _, err := convertAudioToVoiceNote(context.Background(), []byte("not audio")); err == nil {
		t.Fatal("convertAudioToVoiceNote accepted invalid audio")
	}
}
